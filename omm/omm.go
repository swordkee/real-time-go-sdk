// Package omm 是 LSEG EMA 高级 API（OmmConsumer / registerClient / onRefreshMsg /
// onUpdateMsg / onStatusMsg / watchlist 重连语义）的**纯 Go 等价实现**。
//
// 分层（见 docs/eta-omm-design.md）：
//
//	应用层   → 订阅 service=<service> / item <ric>，回调拿 BID/ASK（只写业务，不碰字节）
//	omm      → EMA 高级语义：Consumer / RegisterClient / 回调 / login/directory/重连 状态机（纯 Go）
//	rwf      → RWF/OMM 解码（纯 Go，翻译自 ETA C 的 messageDecoders/fieldListDecoder）
//	encode   → RWF 请求编码（纯 Go，翻译自 ETA C 的 messageEncoders）
//	ripc     → RIPC 传输（纯 Go TCP 握手/connack/心跳/收帧）
//	native   → 以上三者的客户端外壳（Connect/Login/Submit/Read/Ping/Shutdown）
//
// 传输是可注入接口（Config.TransportFactory）：默认走纯 Go native；业务方若需要
// 用闭源库做对照，可自行注入实现，本 SDK 自身不含任何 cgo 代码。
//
// 默认路径**零 .so**：CGO_ENABLED=0 可编译，Windows 也能跑。
// 解码/编码/传输全在自己的 Go 源码里——这是「彻底摆脱闭源库」的核心收益。
package omm

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/swordkee/real-time-go-sdk/fields"
	"github.com/swordkee/real-time-go-sdk/native"
	"github.com/swordkee/real-time-go-sdk/rwf"
)

// State 流状态（refresh/status 消息携带），对齐 EMA OmmState。
type State struct {
	Stream uint8
	Data   uint8
	Code   uint8
	Text   string
}

// Msg 交付给客户端的消息（EMA OmmMsg 的等价物）。
type Msg struct {
	Class     uint8             // rwf.MsgClassRefresh / Update / Status
	Domain    uint8             // rwf.DomainMarketPrice / ...
	StreamID  int32             //
	ServiceID uint16            //
	Name      string            // 订阅标的（RIC）
	State     *State            // refresh / status 携带
	Fields    map[string]string // MARKET_PRICE 的字段原文（BID/ASK/TIMACT…）
	// SeqNum 报文序号；nil = 该报文未携带（与 EMA 的 hasSeqNum 语义一致，不补 0）。
	SeqNum *int64
	At     time.Time
}

// Client 是 EMA OmmConsumerClient 的等价接口：应用只实现这三个回调。
type Client interface {
	OnRefreshMsg(Msg)
	OnUpdateMsg(Msg)
	OnStatusMsg(Msg)
}

// ClientFunc 便捷适配器：只填需要的回调（nil = 忽略）。
type ClientFunc struct {
	Refresh func(Msg)
	Update  func(Msg)
	Status  func(Msg)
}

func (f ClientFunc) OnRefreshMsg(m Msg) {
	if f.Refresh != nil {
		f.Refresh(m)
	}
}
func (f ClientFunc) OnUpdateMsg(m Msg) {
	if f.Update != nil {
		f.Update(m)
	}
}
func (f ClientFunc) OnStatusMsg(m Msg) {
	if f.Status != nil {
		f.Status(m)
	}
}

// ReqMsg 订阅请求（EMA ReqMsg 的等价物）。
type ReqMsg struct {
	// ServiceName TREP service 名（如 "EXAMPLE"）；按 Config.ServiceIDs 解析成数字 serviceId。
	ServiceName string
	// Name 订阅标的（RIC 字符串，见字典/服务目录）。
	Name string
	// ServiceID 显式数字 serviceId（>0 时优先，跳过 ServiceNames 解析）。
	ServiceID uint16
	// Domain 域类型，默认 MARKET_PRICE(6)。
	Domain uint8
}

// NewReqMsg 构造订阅请求。
func NewReqMsg() ReqMsg { return ReqMsg{Domain: rwf.DomainMarketPrice} }

// Service 设置 serviceName（链式）。
func (r ReqMsg) Service(name string) ReqMsg { r.ServiceName = name; return r }

// Item 设置标的（链式）。
func (r ReqMsg) Item(name string) ReqMsg { r.Name = name; return r }

// Config EMA Consumer 配置（EMA OmmConsumerConfig 的等价物）。
type Config struct {
	Host        string // TREP host
	Port        int    // TREP port
	Username    string // DACS 用户名（raw 登录必需）
	Password    string // 预留（当前 raw 登录不带）
	Position    string // 预留（DACS position）
	Application string // applicationId，默认 "256"

	DictionaryFile string // RDMFieldDictionary（FID→字段名必需）

	// TransportFactory 可选：注入自定义传输实现（nil = 默认纯 Go native）。
	// 注入实现只需满足 Transport 接口；本 SDK 自身不提供、也不依赖任何闭源库传输。
	TransportFactory func(Config) (Transport, error)

	// ServiceIDs serviceName → 数字 serviceId（EMA 用 directory 发现；这里是配置驱动）。
	ServiceIDs map[string]uint16
	// DefaultServiceID 未在 ServiceIDs 命中时的兜底 serviceId（0 = 不兜底，命中失败即报错）。
	DefaultServiceID uint16

	ReconnectBackoff time.Duration // 首次重连退避，默认 1s（指数增长，封顶 30s）
	PingInterval     time.Duration // 主动心跳**发送间隔**，默认 15s
	// PingTimeout 写入 conn request 的**超时窗口**（秒粒度），与 PingInterval 刻意解耦：
	// C 的 pingTimeout 是「对方多久收不到我方数据就断开」的窗口，不是「我方发送间隔」。
	// 缺省 = max(60s, 3×PingInterval)。把 10s 发送间隔同时广播成 10s 超时窗口会让
	// ADS 用 10s 空闲窗口和我们 10s 的 ping 赛跑，30 分钟内被 FIN 8 次（2026-11-23 实测）。
	PingTimeout time.Duration
	Fields      []string     // 要交付的字段，默认 fields.DefaultFields
	Logger      *slog.Logger // 可选；nil = slog.Default()（标准库 log/slog）
}

// subscription 一路订阅（streamId ↔ 请求 ↔ 客户端）。
type subscription struct {
	handle    uint64
	req       ReqMsg
	serviceID uint16
	streamID  int32
	client    Client
	submitted bool // 本次连接是否已发过 item request
}

// Consumer EMA 消费端（EMA OmmConsumer 的等价物）。
//
// 生命周期：NewConsumer → 若干 RegisterClient → Run(ctx)（阻塞，内部 login/订阅/重连）。
type Consumer struct {
	cfg  Config
	dict *rwf.FieldDictionary
	extr *rwf.QuoteExtractor

	mu      sync.Mutex
	subs    map[int32]*subscription
	order   []int32
	nextStr int32
	nextH   uint64
	started bool
	tx      Transport
}

// ReadResult 一次读的结果（与两套传输实现同语义：0=有消息 / -11=暂无 / -13=心跳）。
type ReadResult struct {
	Data []byte
	Ret  int32
	Ping bool
}

// Transport 是 Consumer 依赖的最小传输面。
//
//   - nativeTransport：native（纯 Go RIPC，零 .so，**默认**）
//   - 自定义实现：由 Config.TransportFactory 注入（本 SDK 不随附任何闭源传输）
type Transport interface {
	Connect() error
	Login() error
	Submit(item string, serviceID uint16, streamID int32) error
	Read() (ReadResult, error)
	Ping()
	Shutdown()
}

// NewConsumer 校验配置 + 加载字段字典 + 构造传输。不建立连接。
// 任一必需项缺失/字典不可用 → 立刻 fail-closed 返回错误。
//
// 传输选择：
//   - 默认：**纯 Go**（native）——零 .so、CGO_ENABLED=0 可编译、Windows 也能跑。
//   - Config.TransportFactory 非 nil：用注入实现（例如业务方自己的对照传输）。
func NewConsumer(cfg Config) (*Consumer, error) {
	if strings.TrimSpace(cfg.Host) == "" || cfg.Port <= 0 {
		return nil, fmt.Errorf("omm: host/port 未配置（TREP 地址）")
	}
	if strings.TrimSpace(cfg.Username) == "" {
		return nil, fmt.Errorf("omm: username 未配置（DACS 登录名）")
	}
	if strings.TrimSpace(cfg.DictionaryFile) == "" {
		return nil, fmt.Errorf("omm: dictionaryFile 未配置（RDMFieldDictionary，FID→字段名必需）")
	}
	if cfg.Application == "" {
		cfg.Application = "256"
	}
	if len(cfg.Fields) == 0 {
		cfg.Fields = fields.DefaultFields
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = time.Second
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 15 * time.Second
	}
	if cfg.PingTimeout <= 0 {
		// 超时窗口必须明显大于发送间隔；否则服务端按我们广告的短窗口 FIN。
		cfg.PingTimeout = 60 * time.Second
		if want := 3 * cfg.PingInterval; want > cfg.PingTimeout {
			cfg.PingTimeout = want
		}
	}
	dict, err := rwf.LoadFieldDictionary(cfg.DictionaryFile)
	if err != nil {
		return nil, err
	}
	tx, err := cfg.newTransport()
	if err != nil {
		return nil, err
	}
	return &Consumer{
		cfg:     cfg,
		dict:    dict,
		extr:    rwf.NewQuoteExtractor(dict, cfg.Fields),
		subs:    make(map[int32]*subscription),
		nextStr: itemStreamBase,
		tx:      tx,
	}, nil
}

// itemStreamBase item streamId 起点=3（避让 login stream 1 + directory stream 2）。
const itemStreamBase = 3

// RegisterClient 注册一路订阅（EMA registerClient），返回句柄。
//
// 与 EMA 一致：可在 Run 之前调用（排队，登录后统一提交），也可在 Run 期间调用
// （下一个 dispatch 周期提交）。重连后所有订阅会自动重发（watchlist 语义）。
func (c *Consumer) RegisterClient(req ReqMsg, client Client) (uint64, error) {
	if client == nil {
		return 0, fmt.Errorf("omm: client 为空")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return 0, fmt.Errorf("omm: 订阅标的 name 为空")
	}
	if req.Domain == 0 {
		req.Domain = rwf.DomainMarketPrice
	}
	serviceID, err := c.resolveServiceID(req)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextH++
	sid := c.nextStr
	c.nextStr++
	sub := &subscription{
		handle:    c.nextH,
		req:       req,
		serviceID: serviceID,
		streamID:  sid,
		client:    client,
		submitted: false,
	}
	c.subs[sid] = sub
	c.order = append(c.order, sid)
	return sub.handle, nil
}

// resolveServiceID 解析数字 serviceId：显式 > ServiceIDs[name] > DefaultServiceID；否则报错。
func (c *Consumer) resolveServiceID(req ReqMsg) (uint16, error) {
	if req.ServiceID > 0 {
		return req.ServiceID, nil
	}
	if id, ok := c.cfg.ServiceIDs[req.ServiceName]; ok && id > 0 {
		return id, nil
	}
	if c.cfg.DefaultServiceID > 0 {
		return c.cfg.DefaultServiceID, nil
	}
	return 0, fmt.Errorf("omm: 无法解析 service %q 的数字 serviceId（配置 serviceIDs 或显式 ReqMsg.ServiceID）", req.ServiceName)
}

// newTransport 按 Config 选传输实现（默认纯 Go native；TransportFactory 非 nil 时用注入实现）。
func (cfg Config) newTransport() (Transport, error) {
	if cfg.TransportFactory != nil {
		return cfg.TransportFactory(cfg)
	}
	// PingTimeout 是广播给服务端的窗口；PingInterval 只决定我们多久发一次 ping。
	pingTimeout := cfg.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = 60 * time.Second
		if want := 3 * cfg.PingInterval; want > pingTimeout {
			pingTimeout = want
		}
	}
	nc, err := native.New(native.Config{
		Host:           cfg.Host,
		Port:           cfg.Port,
		Username:       cfg.Username,
		Position:       cfg.Position,
		Application:    cfg.Application,
		PingTimeout:    pingTimeout,
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return nativeTransport{conn: nc}, nil
}
