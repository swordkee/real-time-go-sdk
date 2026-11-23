// Package native 是「纯 Go ETA 传输」的对外客户端外壳：RIPC（ripc 包）+ RWF 请求编码
// （rwf/encode 包）+ 登录/订阅/读帧编排，**零 cgo / 零 .so / 零外部库运行时依赖**。
//
// 方法集合与经典 C 客户端的 raw 接口刻意保持一致（Connect/Login/Submit/Read/Ping/Shutdown），
// 方便 omm 层无感切换。
//
//	Conn.Connect()                       （ripc.Dial 到 ACTIVE）
//	Conn.Login()                         （raw REQUEST/LOGIN）
//	Conn.Submit(item, serviceId, stream) （MARKET_PRICE request）
//	Conn.Read()                          （非阻塞；Ping/WouldBlock 同语义）
//	Conn.Ping()
//	Conn.Shutdown()
//
// 读结果 Ret 取值与 C 侧 rsslRead 对齐：0=有消息 / -11=暂无数据（WOULD_BLOCK）/ -13=收到心跳。
package native

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/swordkee/real-time-go-sdk/ripc"
	"github.com/swordkee/real-time-go-sdk/rwf/encode"
)

// rsslRead 兼容返回码。
const (
	RetSuccess    int32 = 0
	RetWouldBlock int32 = -11
	RetReadPing   int32 = -13
)

// LoginStreamID 登录流固定用 stream 1（item 从 3 起，避让 directory 流 2）。
const LoginStreamID int32 = 1

// Config 连接参数（全部由调用方注入，源码不硬编码任何环境信息）。
type Config struct {
	Host     string
	Port     int
	Username string
	// Position/Application 预留：raw 登录报文不携带（与 eta.RawConn 一致，ADS 实测不需要）。
	Position    string
	Application string
	// ConnectTimeout 拨号+握手超时，默认 10s。
	ConnectTimeout time.Duration
	// PingTimeout 写入 conn request 的心跳间隔（秒粒度），默认 60s。
	PingTimeout time.Duration
	// Logf 可选日志。
	Logf func(format string, args ...any)
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" || c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("native: host/port 未配置（TREP 地址）")
	}
	if strings.TrimSpace(c.Username) == "" {
		return fmt.Errorf("native: username 未配置（DACS 登录名）")
	}
	return nil
}

// ReadResult 一次读的结果（与 eta.ReadResult 同语义）。
type ReadResult struct {
	Data []byte
	Ret  int32
	Ping bool
}

// Conn 纯 Go ETA 连接。
type Conn struct {
	cfg Config

	mu  sync.Mutex
	rc  *ripc.Conn
	up  bool
	ini bool
}

// New 校验参数；不建立连接。
func New(cfg Config) (*Conn, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = 60 * time.Second
	}
	return &Conn{cfg: cfg}, nil
}

// Connect 建 TCP + 完成 RIPC 握手（到 ACTIVE）。
func (c *Conn) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.up {
		return fmt.Errorf("native: 已连接，不能重复 Connect")
	}
	rcfg := ripc.DefaultConfig()
	rcfg.Host = c.cfg.Host
	rcfg.Port = c.cfg.Port
	rcfg.Timeout = c.cfg.ConnectTimeout
	rcfg.PingTimeout = c.cfg.PingTimeout
	rcfg.Logf = c.cfg.Logf
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.ConnectTimeout)
	defer cancel()
	rc, err := ripc.Dial(ctx, rcfg)
	if err != nil {
		return err
	}
	c.rc = rc
	c.up = true
	c.ini = true
	if c.cfg.Logf != nil {
		major, minor := rc.RWFVersion()
		c.cfg.Logf("native: 已连接 %s:%d ripc=%d maxUserMsg=%d rwf=%d.%d",
			c.cfg.Host, c.cfg.Port, rc.RipcVersion(), rc.MaxUserMsgSize(), major, minor)
	}
	return nil
}

// Login 发送 raw REQUEST/LOGIN（msgKey.name = username）。
func (c *Conn) Login() error {
	rc, err := c.conn()
	if err != nil {
		return err
	}
	b, err := encode.LoginRequest(LoginStreamID, c.cfg.Username)
	if err != nil {
		return err
	}
	if err := rc.WriteMessage(b); err != nil {
		return fmt.Errorf("native: 发送 login 失败: %w", err)
	}
	if c.cfg.Logf != nil {
		c.cfg.Logf("native: login 请求已发送（%d 字节）", len(b))
	}
	return nil
}

// Submit 发送一条 MARKET_PRICE item request（streamID 由调用方分配）。
func (c *Conn) Submit(item string, serviceID uint16, streamID int32) error {
	rc, err := c.conn()
	if err != nil {
		return err
	}
	b, err := encode.MarketPriceRequest(streamID, serviceID, item)
	if err != nil {
		return err
	}
	if err := rc.WriteMessage(b); err != nil {
		return fmt.Errorf("native: 发送 item request 失败: %w", err)
	}
	if c.cfg.Logf != nil {
		c.cfg.Logf("native: item request 已发送 item=%s serviceId=%d streamId=%d（%d 字节）",
			item, serviceID, streamID, len(b))
	}
	return nil
}

// CloseStream 发送 CLOSE（取消一条订阅；EMA watchlist 关闭流时用）。
func (c *Conn) CloseStream(streamID int32, domain uint8) error {
	rc, err := c.conn()
	if err != nil {
		return err
	}
	return rc.WriteMessage(encode.CloseRequest(streamID, domain))
}

// Read 非阻塞读：有消息返回 Data；收到心跳返回 Ping=true/Ret=-13；没数据 Ret=-11。
func (c *Conn) Read() (ReadResult, error) {
	rc, err := c.conn()
	if err != nil {
		return ReadResult{}, err
	}
	data, ping, err := rc.Read()
	if err != nil {
		return ReadResult{}, err
	}
	switch {
	case ping:
		return ReadResult{Ret: RetReadPing, Ping: true}, nil
	case data != nil:
		return ReadResult{Data: data, Ret: RetSuccess}, nil
	default:
		return ReadResult{Ret: RetWouldBlock}, nil
	}
}

// Ping 回/发心跳（3 字节空数据帧）。
func (c *Conn) Ping() {
	if rc, err := c.conn(); err == nil {
		if perr := rc.Ping(); perr != nil && c.cfg.Logf != nil {
			c.cfg.Logf("native: ping 失败: %v", perr)
		}
	}
}

// Version 返回协商的 RWF 版本。
func (c *Conn) Version() (major, minor byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rc == nil {
		return 0, 0
	}
	return c.rc.RWFVersion()
}

// Ready 是否已连接。
func (c *Conn) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.up
}

// Shutdown 关连接（幂等）。
func (c *Conn) Shutdown() {
	c.mu.Lock()
	rc := c.rc
	c.rc = nil
	c.up = false
	c.ini = false
	c.mu.Unlock()
	if rc != nil {
		_ = rc.Close()
	}
}

func (c *Conn) conn() (*ripc.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.up || c.rc == nil {
		return nil, fmt.Errorf("native: 未连接")
	}
	return c.rc, nil
}
