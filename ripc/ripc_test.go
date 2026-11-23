package ripc

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// 本文件是纯离线门禁：用 net.Pipe + 手工构造的 RIPC 帧验证握手与收帧，
// 不依赖 .so / 网络。黄金字节按 C 侧 ipcConnecting/ipcWaitAck 布局逐字节手写。

// readPipe 从服务端读一个 RIPC 帧（测试用）。
func readPipe(t *testing.T, nc net.Conn) []byte {
	t.Helper()
	var hdr [2]byte
	if _, err := readFull(nc, hdr[:]); err != nil {
		t.Fatalf("读帧头失败: %v", err)
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	b := make([]byte, n)
	copy(b, hdr[:])
	if _, err := readFull(nc, b[2:]); err != nil {
		t.Fatalf("读帧体失败: %v", err)
	}
	return b
}

func readFull(nc net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := nc.Read(b[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func writeAll(t *testing.T, nc net.Conn, b []byte) {
	t.Helper()
	if _, err := nc.Write(b); err != nil {
		t.Fatalf("写失败: %v", err)
	}
}

// connAck 构造服务端 connack。withKeyExchange=true 时按 v14 带 P/G/send_key（27 字节段）。
func connAck(ripcVersion uint32, maxMsgSize uint16, ping byte, withKeyExchange bool) []byte {
	b := []byte{
		0x00, 0x00, // len（稍后回填）
		ipcExtendedFlags, extConnack,
		connAckHdrLen, 0,
	}
	var ver [4]byte
	binary.BigEndian.PutUint32(ver[:], ripcVersion)
	b = append(b, ver[:]...)
	var mms [2]byte
	binary.BigEndian.PutUint16(mms[:], maxMsgSize)
	b = append(b, mms[:]...)
	b = append(b, 0 /* rsslFlags */, ping, 14, 1)
	b = append(b, 0, 0) // comp=0（未压缩）
	b = append(b, 0)    // zlibCompLevel
	if withKeyExchange {
		p := bigFromHex("FFFFFFFFFFFFFFC5")
		g := big.NewInt(5)
		srvRand := big.NewInt(1234567890123)
		sendKey := new(big.Int).Exp(g, srvRand, p)
		var pb, gb, kb [8]byte
		p.FillBytes(pb[:])
		g.FillBytes(gb[:])
		sendKey.FillBytes(kb[:])
		b = append(b, keyExchange, trSL1, 24)
		b = append(b, pb[:]...)
		b = append(b, gb[:]...)
		b = append(b, kb[:]...)
	}
	b = append(b, 1, 0) // 组件版本段（总长 1、字符串长 0）
	binary.BigEndian.PutUint16(b[0:2], uint16(len(b)))
	return b
}

// bigFromHex 把 8 字节大端 hex 转 big.Int（避免 NewInt 的 int64 溢出）。
func bigFromHex(s string) *big.Int {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return new(big.Int).SetBytes(b)
}

// TestConnRequestGolden 握手请求逐字节对拍 C 侧 ipcConnecting()（v14、无压缩位图）：
//
//	len=32 00 00 00000017 08 1e 00 3c 00 00 0e 01 05 "host1" 08 "10.0.0.9" 01 00
func TestConnRequestGolden(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	cfg.Hostname = "host1"
	cfg.LocalAddress = "10.0.0.9"
	cfg.PingTimeout = 60 * time.Second

	got := make(chan []byte, 1)
	go func() {
		req := readPipe(t, srv)
		got <- req
		writeAll(t, srv, connAck(9, 6144, 60, false))
		drainFrame(srv) // 消费 client key 帧（v14 第三腿；不做断言，避免与 Close 抢读）
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()
	req := <-got
	const want = "0020" + "00" + "00000017" + "08" + "1e" + "00" + "3c" + "00" + "00" +
		"0e" + "01" + "05" + "686f737431" + "08" + "31302e302e302e39" + "01" + "00"
	if gotHex := hex.EncodeToString(req); gotHex != want {
		t.Fatalf("conn request 与 C 布局不一致:\n got=%s\nwant=%s", gotHex, want)
	}
}

// TestHandshakeV14KeyExchange v14 第三腿：客户端必须回 12 字节 key 帧，且 key 在 (0,P) 内。
func TestHandshakeV14KeyExchange(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	cfg.Hostname = "h"
	cfg.LocalAddress = "1.1.1.1"

	keyCh := make(chan []byte, 1)
	go func() {
		_ = readPipe(t, srv) // conn request
		_, _ = srv.Write(connAck(9, 6144, 60, true))
		keyCh <- readPipe(t, srv) // client key
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()
	key := <-keyCh
	if len(key) != 12 || key[2] != keyExchange || key[3] != 8 {
		t.Fatalf("client key 帧格式不对: %x", key)
	}
	p := bigFromHex("FFFFFFFFFFFFFFC5")
	v := new(big.Int).SetBytes(key[4:12])
	if v.Sign() <= 0 || v.Cmp(p) >= 0 {
		t.Fatalf("client key 超出 (0,P): %s", v)
	}
	if c.sharedKey == 0 {
		t.Fatal("shared key 应非 0")
	}
}

// TestReadDataFrame 数据帧：3 字节头 + RWF payload → Read 原样返回。
func TestReadDataFrame(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	payload := []byte{0x00, 0x05, 0x02, 0x01, 0x01, 0x00}
	go func() {
		serveHandshake(t, srv, connAck(9, 6144, 60, false))
		frame := make([]byte, 3+len(payload))
		binary.BigEndian.PutUint16(frame, uint16(len(frame)))
		frame[2] = ipcData
		copy(frame[3:], payload)
		_, _ = srv.Write(frame)
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()

	data, ping, err := waitRead(c, 2*time.Second)
	if err != nil || ping {
		t.Fatalf("应收到数据帧，实际 data=%x ping=%v err=%v", data, ping, err)
	}
	if string(data) != string(payload) {
		t.Fatalf("payload 不一致: %x != %x", data, payload)
	}
}

// TestReadPing 3 字节空帧 = 心跳。
func TestReadPing(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	go func() {
		serveHandshake(t, srv, connAck(9, 6144, 60, false))
		_, _ = srv.Write([]byte{0x00, 0x03, ipcData})
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()

	data, ping, err := waitRead(c, 2*time.Second)
	if err != nil || !ping || data != nil {
		t.Fatalf("应收到 ping，实际 data=%x ping=%v err=%v", data, ping, err)
	}
	// 回 ping 必须是 00 03 02。
	got := make(chan []byte, 1)
	go func() { got <- readPipe(t, srv) }()
	if err := c.Ping(); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
	if b := <-got; hex.EncodeToString(b) != "000302" {
		t.Fatalf("ping 帧不对: %x", b)
	}
}

// TestReadPacked packing 帧：[u16 len][msg]×2 → 两条独立消息。
func TestReadPacked(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	m1 := []byte{0xAA, 0x01}
	m2 := []byte{0xBB, 0x02, 0x03}
	go func() {
		serveHandshake(t, srv, connAck(9, 6144, 60, false))
		body := append([]byte{0x00, byte(len(m1))}, m1...)
		body = append(body, 0x00, byte(len(m2)))
		body = append(body, m2...)
		frame := make([]byte, 3+len(body))
		binary.BigEndian.PutUint16(frame, uint16(len(frame)))
		frame[2] = ipcData | ipcPacking
		copy(frame[3:], body)
		_, _ = srv.Write(frame)
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()

	d1, _, err := waitRead(c, 2*time.Second)
	if err != nil || string(d1) != string(m1) {
		t.Fatalf("第一条 packed 消息不对: %x err=%v", d1, err)
	}
	d2, _, err := waitRead(c, 2*time.Second)
	if err != nil || string(d2) != string(m2) {
		t.Fatalf("第二条 packed 消息不对: %x err=%v", d2, err)
	}
}

// TestReadFragmented 分片：FRAG_HEADER(总长+fragId) + FRAG(fragId) → 拼成一条消息。
func TestReadFragmented(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	full := []byte("hello-fragmented-world")
	part1 := full[:9]
	part2 := full[9:]
	go func() {
		serveHandshake(t, srv, connAck(9, 6144, 60, false))

		f1 := make([]byte, 10+len(part1))
		binary.BigEndian.PutUint16(f1, uint16(len(f1)))
		f1[2] = ipcData | ipcExtendedFlags
		f1[3] = extFragHeader
		binary.BigEndian.PutUint32(f1[4:8], uint32(len(full)))
		binary.BigEndian.PutUint16(f1[8:10], 7) // fragId
		copy(f1[10:], part1)
		_, _ = srv.Write(f1)

		f2 := make([]byte, 6+len(part2))
		binary.BigEndian.PutUint16(f2, uint16(len(f2)))
		f2[2] = ipcData | ipcExtendedFlags
		f2[3] = extFrag
		binary.BigEndian.PutUint16(f2[4:6], 7)
		copy(f2[6:], part2)
		_, _ = srv.Write(f2)
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()

	data, _, err := waitRead(c, 2*time.Second)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if string(data) != string(full) {
		t.Fatalf("装配结果不对: %q", data)
	}
}

// TestConnNak 对端 NAK：错误里必须带服务端文本。
func TestConnNak(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	go func() {
		_ = readPipe(t, srv)
		text := []byte("no permission")
		b := []byte{0, 0, ipcExtendedFlags, extConnNak, 0, 0, 0, byte(len(text))}
		b = append(b, text...)
		binary.BigEndian.PutUint16(b[:2], uint16(len(b)))
		_, _ = srv.Write(b)
	}()

	_, err := New(cli, cfg)
	if err == nil || !errors.Is(err, ErrConnNak) || !strings.Contains(err.Error(), "no permission") {
		t.Fatalf("应返回带文本的 ErrConnNak，实际 %v", err)
	}
}

// TestCompressedFrameRejected 未协商压缩却收到压缩帧 → fail-closed。
func TestCompressedFrameRejected(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	go func() {
		serveHandshake(t, srv, connAck(9, 6144, 60, false))
		_, _ = srv.Write([]byte{0x00, 0x05, ipcData | ipcCompData, 0x01, 0x02})
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()
	if _, _, err := waitRead(c, 2*time.Second); !errors.Is(err, ErrProtocol) {
		t.Fatalf("压缩帧应报 ErrProtocol，实际 %v", err)
	}
}

// TestServerCompressionUnknownRejected connack 指定未知压缩类型(3) → 握手 fail-closed。
// （已知类型 1/2 现在支持，见 compress_test.go。）
func TestServerCompressionUnknownRejected(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	go func() {
		_ = readPipe(t, srv)
		ack := connAck(9, 6144, 60, false)
		// comp 字段在偏移 16..17
		binary.BigEndian.PutUint16(ack[16:18], 3)
		_, _ = srv.Write(ack)
	}()

	if _, err := New(cli, cfg); !errors.Is(err, ErrProtocol) {
		t.Fatalf("未知压缩类型应报 ErrProtocol，实际 %v", err)
	}
}

// TestWriteMessageHeader WriteMessage 的帧头（length/opcode）正确且整帧写入。
func TestWriteMessageHeader(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	got := make(chan []byte, 1)
	go func() {
		serveHandshake(t, srv, connAck(9, 6144, 60, false))
		got <- readPipe(t, srv)
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()
	payload := []byte{1, 2, 3, 4}
	if err := c.WriteMessage(payload); err != nil {
		t.Fatalf("WriteMessage 失败: %v", err)
	}
	frame := <-got
	if len(frame) != 7 || frame[2] != ipcData || string(frame[3:]) != string(payload) {
		t.Fatalf("帧不对: %x", frame)
	}
}

// tryReadFrame 读一帧；失败返回 error（给 goroutine 用，避免 t.Fatalf 跨 goroutine）。
func tryReadFrame(c net.Conn) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	var hdr [2]byte
	if _, err := readFull(c, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n <= 2 {
		return nil, nil
	}
	buf := make([]byte, n-2)
	if _, err := readFull(c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// drainFrame 读并丢弃一帧（不报错：可能与 Close 竞争，测试收尾时读失败是正常的）。
func drainFrame(c net.Conn) { _, _ = tryReadFrame(c) }

// serveHandshake 扮演服务端完成握手：读 conn request → 写 connack → 读 client key 帧
// （v14 客户端恒发第三腿：有 P/G 则真 key，无则 0 长度 key，与 C 行为一致）。
func serveHandshake(t *testing.T, srv net.Conn, ack []byte) {
	t.Helper()
	if _, err := tryReadFrame(srv); err != nil {
		t.Errorf("服务端读 conn request 失败: %v", err)
		return
	}
	writeAll(t, srv, ack)
	drainFrame(srv) // client key 帧
}

// tcpPair 起一对真实 loopback TCP 连接（取代 net.Pipe：后者的 deadline 两端共享、
// 且写是同步的，容易与 bufio 抢读造成假死；TCP 更贴近生产且各端 deadline 独立）。
func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 失败: %v", err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("accept 失败: %v", r.err)
	}
	return r.c, cli
}

// TestReadAfterCloseReportsClosed 关闭后 Read 必须报 ErrClosed（不能返回 nil 让上层空转）。
func TestReadAfterCloseReportsClosed(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	go func() { serveHandshake(t, srv, connAck(9, 6144, 60, false)) }()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 给读循环一点时间退出。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, rerr := c.Read(); rerr != nil {
			if !errors.Is(rerr, ErrClosed) {
				t.Fatalf("关闭后 Read 应报 ErrClosed，实际 %v", rerr)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("关闭后 Read 一直返回 nil（上层会空转）")
}

// TestWriteOversizeRejected 超过协商 maxUserMsgSize 的消息必须拒绝（fail-closed）。
func TestWriteOversizeRejected(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second
	go func() { serveHandshake(t, srv, connAck(9, 64, 60, false)) }()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()
	if err := c.WriteMessage(make([]byte, 65)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("超长消息应报 ErrProtocol，实际 %v", err)
	}
}

// waitRead 轮询 Read 直到有事件/错误/超时。
func waitRead(c *Conn, d time.Duration) ([]byte, bool, error) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		data, ping, err := c.Read()
		if err != nil || data != nil || ping {
			return data, ping, err
		}
		time.Sleep(time.Millisecond)
	}
	return nil, false, errors.New("等待读超时")
}
