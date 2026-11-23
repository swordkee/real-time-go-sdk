// Package ripc 是 LSEG RIPC（Reuters Inter-Process Communication）**纯 Go 传输层**实现：
// TCP + 连接握手（conn request / connack / v14 key exchange）+ 数据帧（含 extended flags、
// packing、fragmentation、ping），不依赖 cgo / 任何闭源库。
//
// 逐条对照 RTSDK C 的 Impl/Transport/rsslSocketTransportImpl.c：
//   - ipcConnecting()      客户端 conn request（CONN_VERSION_10..14 布局、组件版本段）
//   - ipcWaitAck()         客户端解析 connack（NAK 文本、maxMsgSize、pingTimeout、压缩、
//     v14 key exchange 的 P/G/send_key、组件版本段）
//   - ipcSendClientKey()   v14 第三腿：client_send_key = G^r mod P
//   - ipcReadSession()     数据帧：3 字节头（uint16 len + uint8 flags/opcode）、extended flags、
//     fragmentation（v13+ fragId 为 2 字节）、packing（[uint16 len][msg]…）
//   - ipcIntWrtHeader()    ping = 仅 3 字节头（len=3, opcode=IPC_DATA），无 payload
//
// 设计取舍：
//   - **不请求压缩**（conn request 的压缩位图长度 = 0），但**支持服务端强制压缩**：
//     connack 帧 flags 的 RSSL_COMP_FORCE(0x80) 或 comp 字段指定类型时，按 C 语义启用
//     解压（zlib 持续流 / LZ4 block，见 compress.go）；非法类型 fail-closed。
//     发送侧永不压缩（C 的 zlib 发送阈值 = maxMsgSize，请求远小于它）。
//   - 读侧用独立 goroutine 把完整 RWF 消息投进带缓冲 channel；Read() 非阻塞取。
//   - 所有长度/装配都做越界校验（fail-closed），不信任对端。
package ripc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// RIPC 帧头标志（ripc_int.h）。
const (
	ipcExtendedFlags = 0x01 // 后面还有 1 字节 extended flags
	ipcData          = 0x02 // 普通未压缩数据（完整性校验位）
	ipcCompData      = 0x04 // 压缩数据（服务端强制压缩时启用）
	ipcCompFrag      = 0x08 // 压缩分片（压缩数据被拆成两帧，LZ4 大消息）
	ipcPacking       = 0x10 // 本帧内打包了多条消息
)

// extended flags（ripc_int.h，仅当 ipcExtendedFlags 置位时出现）。
const (
	extConnack     = 0x01
	extConnNak     = 0x02
	extFrag        = 0x04 // 后续分片（带 fragId）
	extFragHeader  = 0x08 // 首片（带总长 + fragId）
	connVer10      = 0x13
	connVer11      = 0x14
	connVer12      = 0x15
	connVer13      = 0x16
	connVer14      = 0x17
	keyExchange    = 0x08     // CONN_VERSION_14 的 key exchange 标志（请求 flags 位）
	trSL1          = 1        // 唯一支持的 key exchange 类型
	connAckHdrLen  = 10       // connack 的 headerLength 字段值（IPC_100_CONN_ACK）
	dataHeaderLen  = 3        // 数据帧固定头长度
	connReqMinHdr  = 14       // V10_MIN_CONN_HDR
	connAckCommonH = 18       // IPC_100_CONN_ACK_AND_COMMON_HEADERS
	maxFrameBudget = 64 << 20 // 单条（分片装配后）消息上限，防对端 OOM
)

// 错误。
var (
	// ErrConnNak 对端 NAK（连接被拒，通常带文本）。
	ErrConnNak = errors.New("ripc: 连接被服务端拒绝（CONNNAK）")
	// ErrProtocol RIPC 协议错误（帧格式/压缩/分片不一致）。
	ErrProtocol = errors.New("ripc: RIPC 协议错误")
	// ErrClosed 连接已关闭。
	ErrClosed = errors.New("ripc: 连接已关闭")
)

// DefaultConfig 默认参数（真实环境应由配置注入，不在源码硬编码用户名/地址）。
func DefaultConfig() Config {
	return Config{
		PingTimeout:  60 * time.Second,
		Timeout:      10 * time.Second,
		ConnVersion:  connVer14,
		MajorVersion: 14, // RWF 编解码版本（rsslIterators.h RSSL_RWF_MAJOR_VERSION）
		MinorVersion: 1,
	}
}

// Config 连接参数。
type Config struct {
	Host string
	Port int
	// PingTimeout 写入 conn request 的心跳间隔（秒，1..255）；对端也可能改写（见 PeerPingTimeout）。
	PingTimeout time.Duration
	// Timeout TCP 拨号 + 握手的总超时。
	Timeout time.Duration
	// ConnVersion RIPC 连接版本：默认 connVer14(0x17)。仅调试用，一般不改。
	ConnVersion uint32
	// MajorVersion/MinorVersion RWF 版本（14.1）。
	MajorVersion byte
	MinorVersion byte
	// Hostname / LocalAddress 写入 conn request 的客户端信息（默认取 os.Hostname / 本地地址）。
	// 测试里可显式指定以做逐字节断言。
	Hostname     string
	LocalAddress string
	// Logf 可选日志（握手/异常时调用，可为 nil）。
	Logf func(format string, args ...any)
}

func (c Config) validate() error {
	if c.Host == "" || c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("ripc: host/port 非法（%q:%d）", c.Host, c.Port)
	}
	if c.PingTimeout <= 0 {
		c.PingTimeout = 60 * time.Second
	}
	return nil
}

// PingSeconds 把 PingTimeout 折成 1 字节（C 侧 IPC_MINIMUM_PINGTIMEOUT=1 / MAXIMUM=0xFF）。
func (c Config) pingSeconds() byte {
	s := int(c.PingTimeout / time.Second)
	switch {
	case s <= 0:
		return 1
	case s > 0xFF:
		return 0xFF
	}
	return byte(s)
}

type event struct {
	data []byte
	ping bool
}

// Conn 一条已握手的 RIPC 连接。
type Conn struct {
	cfg Config
	nc  net.Conn
	br  *bufio.Reader

	wmu sync.Mutex // 串行写（协议要求整帧原子写）

	events    chan event
	done      chan struct{}
	closeOnce sync.Once

	mu  sync.Mutex
	err error

	// 协商结果（握手后只读）。
	ripcVersion   uint32
	maxUserMsg    uint16
	negotiatedRWF [2]byte
	peerPing      byte
	compression   uint16

	// 压缩传输（connack 指定后启用；发送侧永不压缩——conn request 的压缩位图为空，
	// 且 C 侧 zlib 发送阈值 = maxMsgSize，我们的请求远小于它）。
	decomp decompressor
	// compPending 对应 C 的 tempDecompressBuf：IPC_COMP_FRAG 的第一段先缓存，第二段到达后一起解压。
	compPending []byte

	// v14 key exchange（connack 里下发的 P/G/send_key；sharedKey 仅保留以便未来应用层加解密）。
	exType    byte
	pBytes    []byte
	gBytes    []byte
	sendKey   []byte
	sharedKey uint64

	// 分片装配（fragId → 缓冲）。
	asmMu sync.Mutex
	asm   map[uint16]*assembly
}

// Dial 建 TCP + 完成 RIPC 握手（到 ACTIVE）；失败时关闭连接并返回错误。
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	cfg = withDefaults(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	d := &net.Dialer{Timeout: cfg.Timeout}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("ripc: 连接 %s:%d 失败: %w", cfg.Host, cfg.Port, err)
	}
	c, err := New(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

// New 在已建立的 net.Conn 上完成握手（测试/复用连接用）。握手超时用 cfg.Timeout。
func New(nc net.Conn, cfg Config) (*Conn, error) {
	cfg = withDefaults(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	c := &Conn{
		cfg:    cfg,
		nc:     nc,
		br:     bufio.NewReaderSize(nc, 32*1024),
		events: make(chan event, 256),
		done:   make(chan struct{}),
		asm:    make(map[uint16]*assembly),
	}
	if err := nc.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		return nil, fmt.Errorf("ripc: 设置握手超时失败: %w", err)
	}
	if err := c.handshake(); err != nil {
		return nil, err
	}
	// 握手完成后清掉 deadline：读循环用阻塞读，由 Close 打断。
	if err := nc.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("ripc: 清除超时失败: %w", err)
	}
	go c.readLoop()
	return c, nil
}

func withDefaults(cfg Config) Config {
	d := DefaultConfig()
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = d.PingTimeout
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = d.Timeout
	}
	if cfg.ConnVersion == 0 {
		cfg.ConnVersion = d.ConnVersion
	}
	if cfg.MajorVersion == 0 && cfg.MinorVersion == 0 {
		cfg.MajorVersion, cfg.MinorVersion = d.MajorVersion, d.MinorVersion
	}
	return cfg
}

func (c *Conn) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf("ripc: "+format, args...)
	}
}

// ---------------------------------------------------------------- 握手

// handshake 发 conn request → 读 connack →（v14）key exchange。
func (c *Conn) handshake() error {
	req := c.buildConnRequest()
	if err := c.writeRaw(req); err != nil {
		return fmt.Errorf("ripc: 发送 conn request 失败: %w", err)
	}
	ack, err := c.readRawFrame()
	if err != nil {
		return fmt.Errorf("ripc: 读 connack 失败: %w", err)
	}
	if err := c.processConnAck(ack); err != nil {
		return err
	}
	if c.cfg.ConnVersion > connVer13 {
		if err := c.sendClientKey(); err != nil {
			return err
		}
	}
	c.logf("握手完成 ripcVersion=%d maxUserMsg=%d rwf=%d.%d peerPingTimeout=%ds comp=%d",
		c.ripcVersion, c.maxUserMsg, c.negotiatedRWF[0], c.negotiatedRWF[1], c.peerPing, c.compression)
	return nil
}

// buildConnRequest 对照 ipcConnecting()（CONN_VERSION_10..14 客户端分支）。
func (c *Conn) buildConnRequest() []byte {
	hostname := c.cfg.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	addr := c.cfg.LocalAddress
	if addr == "" {
		if la, ok := c.nc.LocalAddr().(*net.TCPAddr); ok && la != nil && la.IP != nil {
			addr = la.IP.String()
		}
	}
	compVerLen := 0 // 无组件版本字符串（C 默认 componentVerLen=0）
	const bitmapLen = 0

	hdrSize := connReqMinHdr + len(hostname) + len(addr) + 2 // +2：hostnameLen/addrLen 两个长度字节
	if c.cfg.ConnVersion > connVer11 {
		hdrSize++ // v12+ 多一个 protocolType 字节
	}
	compVerTotal := compVerLen + 1 // 组件版本总长度字节（1 + 字符串长度）
	total := hdrSize + compVerTotal + 1

	b := make([]byte, 0, total)
	b = append(b, 0, 0) // len 稍后回填
	b = append(b, 0)    // opCode = 0（streaming 连接请求）
	b = binary.BigEndian.AppendUint32(b, c.cfg.ConnVersion)
	flags := byte(0)
	if c.cfg.ConnVersion > connVer13 {
		flags = keyExchange
	}
	b = append(b, flags)
	b = append(b, byte(hdrSize))
	b = append(b, bitmapLen) // 压缩位图长度 = 0（不协商压缩）
	b = append(b, c.cfg.pingSeconds())
	b = append(b, 0) // rsslFlags（客户端恒 0）
	if c.cfg.ConnVersion > connVer11 {
		b = append(b, 0) // protocolType = RWF(0)
	}
	b = append(b, c.cfg.MajorVersion, c.cfg.MinorVersion)
	b = append(b, byte(len(hostname)))
	b = append(b, hostname...)
	b = append(b, byte(len(addr)))
	b = append(b, addr...)
	b = append(b, byte(compVerTotal), byte(compVerLen)) // 组件版本段（空字符串）
	b[0] = byte(len(b) >> 8)
	b[1] = byte(len(b))
	return b
}

// processConnAck 对照 ipcWaitAck() 的解析分支。
func (c *Conn) processConnAck(b []byte) error {
	if len(b) < 6 {
		return fmt.Errorf("%w: connack 过短（%d 字节）", ErrProtocol, len(b))
	}
	flags := b[2]
	if flags&ipcExtendedFlags == 0 {
		return fmt.Errorf("%w: connack 缺 extended flags（flags=0x%02x）", ErrProtocol, flags)
	}
	if len(b) < 4 {
		return fmt.Errorf("%w: connack 缺 extended flags 字节", ErrProtocol)
	}
	opCode := b[3]
	if opCode&extConnNak != 0 {
		text := ""
		if len(b) > 8 {
			n := int(binary.BigEndian.Uint16(b[6:8]))
			if n > 0 && 8+n <= len(b) {
				text = string(b[8 : 8+n])
			}
		}
		return fmt.Errorf("%w: %s", ErrConnNak, text)
	}
	if opCode&extConnack == 0 {
		return fmt.Errorf("%w: connack 缺 CONNACK 位（opCode=0x%02x）", ErrProtocol, opCode)
	}
	if len(b) < connAckCommonH {
		return fmt.Errorf("%w: connack 长度不足（%d < %d）", ErrProtocol, len(b), connAckCommonH)
	}
	c.ripcVersion = binary.BigEndian.Uint32(b[6:10])
	switch c.ripcVersion {
	case 5, 6, 7, 8, 9: // RIPC_VERSION_10..14
	default:
		return fmt.Errorf("%w: 未知 RIPC 版本 %d", ErrProtocol, c.ripcVersion)
	}
	c.maxUserMsg = binary.BigEndian.Uint16(b[10:12])
	if c.maxUserMsg == 0 {
		return fmt.Errorf("%w: connack maxMsgSize=0", ErrProtocol)
	}
	c.peerPing = b[13]
	c.negotiatedRWF = [2]byte{b[14], b[15]}
	c.compression = binary.BigEndian.Uint16(b[16:18])
	// C ipcWaitAck（RIPC_VERSION_11..14）：if ((flags & RSSL_COMP_FORCE) && (!comp)) comp = RSSL_COMP_ZLIB;
	// 注意 flags 是 connack 的**帧 flags 字节**（buf[2]），不是 buf[12] 的 rsslFlags。
	if flags&compForce != 0 && c.compression == 0 {
		c.compression = compZlib
	}
	if c.compression > compLZ4 {
		return fmt.Errorf("%w: 服务端指定未知压缩类型 %d", ErrProtocol, c.compression)
	}
	c.decomp = newDecompressor(byte(c.compression), int(c.maxUserMsg))
	if c.ripcVersion > 8 { // RIPC_VERSION_13 以上带 key exchange + 组件版本段
		c.parseKeyExchange(b)
	}
	return nil
}

// parseKeyExchange 从 connack 里取 P/G/send_key（v14 第三腿用）。
func (c *Conn) parseKeyExchange(b []byte) {
	// 布局：19 字节公共头/压缩级别 + 1 字节 KEY_EXCHANGE 标记 + 1 字节类型 + 1 字节长度 +
	// 8 字节 P + 8 字节 G + 8 字节 send_key（与 C ipcWaitAck 的 tempIter 完全一致）。
	if len(b) < 22 {
		return
	}
	if b[19] != keyExchange { // 服务端没做 key exchange
		return
	}
	encType := b[20]
	if encType != trSL1 || len(b) < 22+24 {
		return
	}
	c.exType = encType
	c.pBytes = append([]byte(nil), b[22:30]...)
	c.gBytes = append([]byte(nil), b[30:38]...)
	c.sendKey = append([]byte(nil), b[38:46]...)
}

// sendClientKey 对照 ipcSendClientKey()：算 client_send_key 并发 12 字节帧。
//
// 与 C 的对应关系：
//   - C 只在 encryptionType == TR_SL_1 && P != 0 时计算；否则回 0 长度 key 帧（并清 shared_key）；
//   - 随机数 C 用 randull()（全 64 位，无区间约束）；这里用 crypto/rand 取全 64 位，
//     协议字节与 C 完全同构（随机源更强，不影响互通）。
func (c *Conn) sendClientKey() error {
	if c.exType != trSL1 || len(c.pBytes) != 8 {
		// 与 C 一致：不知道对端类型时回 0 长度 key。
		return c.writeRaw([]byte{0x00, 0x04, keyExchange, 0x00})
	}
	p := new(big.Int).SetBytes(c.pBytes)
	g := new(big.Int).SetBytes(c.gBytes)
	srv := new(big.Int).SetBytes(c.sendKey)
	if p.Sign() == 0 { // C: P != 0
		return c.writeRaw([]byte{0x00, 0x04, keyExchange, 0x00})
	}
	var rb [8]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return fmt.Errorf("ripc: key exchange 随机数失败: %w", err)
	}
	r := new(big.Int).SetUint64(binary.BigEndian.Uint64(rb[:]))
	clientKey := new(big.Int).Exp(g, r, p)
	shared := new(big.Int).Exp(srv, r, p)
	var kb [8]byte
	clientKey.FillBytes(kb[:])
	c.sharedKey = shared.Uint64() // 与 C 一样只用于应用层加解密 API；传输不加密
	b := make([]byte, 0, 12)
	b = append(b, 0, 12, keyExchange, 8)
	b = append(b, kb[:]...)
	return c.writeRaw(b)
}

// ---------------------------------------------------------------- 读循环

func (c *Conn) readLoop() {
	for {
		frame, err := c.readRawFrame()
		if err != nil {
			c.fail(err)
			return
		}
		if err := c.dispatchFrame(frame); err != nil {
			c.fail(err)
			return
		}
	}
}

// readRawFrame 读一个完整 RIPC 帧（含 3 字节及以上的头）。
func (c *Conn) readRawFrame() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n < 3 {
		return nil, fmt.Errorf("%w: 帧长度非法 %d", ErrProtocol, n)
	}
	if n > maxFrameBudget {
		return nil, fmt.Errorf("%w: 帧长度超限 %d", ErrProtocol, n)
	}
	b := make([]byte, n)
	copy(b, hdr[:])
	if _, err := io.ReadFull(c.br, b[2:]); err != nil {
		return nil, err
	}
	return b, nil
}

// dispatchFrame 解析数据帧（对照 ipcReadSession + rsslSocketRead 的 unpack/frag 逻辑）。
func (c *Conn) dispatchFrame(b []byte) error {
	opCode := b[2]
	hdrLen := dataHeaderLen
	var extOp byte
	if opCode&ipcExtendedFlags != 0 {
		if len(b) < 4 {
			return fmt.Errorf("%w: extended flags 截断", ErrProtocol)
		}
		extOp = b[3]
		hdrLen = 4
	}
	var fragID uint16
	var fragTotal uint32
	isFragFirst, isFragNext := false, false
	if extOp&extFragHeader != 0 {
		if len(b) < 10 {
			return fmt.Errorf("%w: 分片头截断", ErrProtocol)
		}
		fragTotal = binary.BigEndian.Uint32(b[4:8])
		fragID = binary.BigEndian.Uint16(b[8:10])
		hdrLen = 10
		isFragFirst = true
	} else if extOp&extFrag != 0 {
		if len(b) < 6 {
			return fmt.Errorf("%w: 分片 id 截断", ErrProtocol)
		}
		fragID = binary.BigEndian.Uint16(b[4:6])
		hdrLen = 6
		isFragNext = true
	}
	if hdrLen > len(b) {
		return fmt.Errorf("%w: 帧头长度超帧（%d > %d）", ErrProtocol, hdrLen, len(b))
	}
	payload := b[hdrLen:]

	// 压缩帧：payload 是压缩数据，解压后再按 PACKING 拆分/投递（C: ipcReadSession 的
	// 「decompress before processing the rest of the header」）。
	if opCode&ipcCompData != 0 {
		return c.dispatchCompressed(opCode, payload)
	}

	if isFragFirst {
		if fragTotal == 0 || fragTotal > maxFrameBudget {
			return fmt.Errorf("%w: 分片总长非法 %d", ErrProtocol, fragTotal)
		}
		c.asmMu.Lock()
		c.asm[fragID] = &assembly{total: fragTotal, buf: make([]byte, fragTotal)}
		c.asmMu.Unlock()
		if len(payload) == 0 {
			return nil
		}
		return c.appendFragment(fragID, payload)
	}
	if isFragNext {
		return c.appendFragment(fragID, payload)
	}
	if opCode&ipcPacking != 0 {
		return c.dispatchPacked(payload)
	}
	if len(payload) == 0 {
		c.emit(event{ping: true})
		return nil
	}
	c.emit(event{data: payload})
	return nil
}

// appendFragment 追加分片；装配完成后投递。
func (c *Conn) appendFragment(fragID uint16, p []byte) error {
	c.asmMu.Lock()
	a := c.asm[fragID]
	if a == nil {
		c.asmMu.Unlock()
		return fmt.Errorf("%w: 收到 fragId=%d 的后续分片但无首片", ErrProtocol, fragID)
	}
	if a.got+len(p) > len(a.buf) {
		delete(c.asm, fragID)
		c.asmMu.Unlock()
		return fmt.Errorf("%w: 分片溢出 fragId=%d (%d+%d>%d)", ErrProtocol, fragID, a.got, len(p), len(a.buf))
	}
	copy(a.buf[a.got:], p)
	a.got += len(p)
	done := a.got == len(a.buf)
	var out []byte
	if done {
		out = a.buf
		delete(c.asm, fragID)
	}
	c.asmMu.Unlock()
	if done {
		c.emit(event{data: out})
	}
	return nil
}

// dispatchCompressed 处理压缩数据帧（对照 ipcReadSession 的压缩分支）：
//   - IPC_COMP_FRAG：压缩数据的第一段，先缓存（C 的 tempDecompressBuf），等第二段；
//   - 第二段（只有 IPC_COMP_DATA）：与缓存拼接后整体解压；
//   - 解压后的 payload 再按 PACKING 拆分（压缩与 packing 可以叠加）。
func (c *Conn) dispatchCompressed(opCode byte, payload []byte) error {
	if c.decomp == nil {
		return fmt.Errorf("%w: 未协商压缩却收到压缩帧", ErrProtocol)
	}
	if opCode&ipcCompFrag != 0 {
		if len(c.compPending)+len(payload) > maxFrameBudget {
			return fmt.Errorf("%w: 压缩分片累计超过上限", ErrProtocol)
		}
		c.compPending = append(c.compPending, payload...)
		return nil
	}
	comp := payload
	if len(c.compPending) > 0 {
		comp = make([]byte, 0, len(c.compPending)+len(payload))
		comp = append(comp, c.compPending...)
		comp = append(comp, payload...)
		c.compPending = nil
	}
	out, err := c.decomp.Decompress(comp)
	if err != nil {
		return fmt.Errorf("%w: 解压失败: %v", ErrProtocol, err)
	}
	if opCode&ipcPacking != 0 {
		return c.dispatchPacked(out)
	}
	if len(out) == 0 {
		c.emit(event{ping: true})
		return nil
	}
	c.emit(event{data: out})
	return nil
}

// dispatchPacked 拆 packing 帧：[uint16 len][msg]… （对照 rsslSocketRead 的 unpack 逻辑）。
func (c *Conn) dispatchPacked(p []byte) error {
	for len(p) > 0 {
		if len(p) < 2 {
			return fmt.Errorf("%w: packed 帧尾不足 2 字节", ErrProtocol)
		}
		n := int(binary.BigEndian.Uint16(p[:2]))
		if n == 0 || 2+n > len(p) {
			return fmt.Errorf("%w: packed 消息长度非法 %d（剩余 %d）", ErrProtocol, n, len(p))
		}
		c.emit(event{data: p[2 : 2+n]})
		p = p[2+n:]
	}
	return nil
}

// emit 投递事件；消费者过慢时阻塞（有界背压），连接关闭时退出。
func (c *Conn) emit(e event) {
	select {
	case c.events <- e:
	case <-c.done:
	}
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		var ne net.Error
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), errors.As(err, &ne) && ne.Timeout():
			c.err = ErrClosed
		default:
			c.err = err
		}
	}
	c.mu.Unlock()
	c.closeDone()
}

func (c *Conn) closeDone() {
	c.closeOnce.Do(func() { close(c.done) })
}

// ---------------------------------------------------------------- 对外 API

// Read 非阻塞取一条消息：data = 一条完整 RWF 消息；ping = 收到对端心跳（应回 Ping）；
// 都没数据返回 (nil,false,nil)；连接已断返回错误（正常关闭也返回 ErrClosed，不返回 nil）。
func (c *Conn) Read() (data []byte, ping bool, err error) {
	select {
	case e := <-c.events:
		return e.data, e.ping, nil
	default:
	}
	select {
	case e := <-c.events:
		return e.data, e.ping, nil
	case <-c.done:
		// 断线前可能还有排队消息：先取完再报错。
		select {
		case e := <-c.events:
			return e.data, e.ping, nil
		default:
		}
		if err := c.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, ErrClosed
	default:
		return nil, false, nil
	}
}

// Err 返回连接结束原因（nil = 正常关闭或仍在运行）。
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// WriteMessage 写一条 RWF 消息（加 3 字节 RIPC 头）。payload 超过协商 maxUserMsgSize 报错
// （C 侧限制的是用户消息大小；3 字节 RIPC 头另算）。
func (c *Conn) WriteMessage(payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("%w: 空消息", ErrProtocol)
	}
	if c.maxUserMsg > 0 && len(payload) > int(c.maxUserMsg) {
		return fmt.Errorf("%w: 消息 %d 字节超过协商上限 %d", ErrProtocol, len(payload), c.maxUserMsg)
	}
	b := make([]byte, dataHeaderLen+len(payload))
	total := len(b)
	b[0] = byte(total >> 8)
	b[1] = byte(total)
	b[2] = ipcData
	copy(b[3:], payload)
	return c.writeRaw(b)
}

// Ping 发心跳（3 字节空数据帧，对照 ipcIntWrtHeader）。
func (c *Conn) Ping() error {
	return c.writeRaw([]byte{0x00, 0x03, ipcData})
}

// writeRaw 串行写整段字节（协议要求帧不被并发撕裂）。
func (c *Conn) writeRaw(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.isClosed() {
		return ErrClosed
	}
	if _, err := c.nc.Write(b); err != nil {
		return err
	}
	return nil
}

func (c *Conn) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// Close 关闭连接（幂等）。读循环会以 ErrClosed 收尾。
func (c *Conn) Close() error {
	c.closeDone()
	return c.nc.Close()
}

// 协商结果访问器（诊断/日志用）。
func (c *Conn) RipcVersion() uint32    { return c.ripcVersion }
func (c *Conn) MaxUserMsgSize() uint16 { return c.maxUserMsg }
func (c *Conn) RWFVersion() (byte, byte) {
	return c.negotiatedRWF[0], c.negotiatedRWF[1]
}
func (c *Conn) PeerPingTimeout() time.Duration { return time.Duration(c.peerPing) * time.Second }

// assemblyKeyExchange 与分片装配用的内部字段（放在 Conn 结构补充）。
type assembly struct {
	total uint32
	buf   []byte
	got   int
}
