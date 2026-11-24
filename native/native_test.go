package native

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// 本文件是纯离线门禁：本地 TCP 假服务端扮演 RIPC 对端，验证 native.Conn 的
// Connect/Login/Submit/Read/Ping/Shutdown 与帧字节，不依赖 .so / 外网。

// dialNative 起假服务端 + 返回已握手的 native.Conn 与服务端连接。
func dialNative(t *testing.T) (*Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if _, ok := readFrame(c); !ok {
			return
		}
		_, _ = c.Write(connAckFrame())
		_, _ = readFrame(c) // client key
		accepted <- c
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	port, _ = strconv.Atoi(portStr)
	cn, err := New(Config{Host: host, Port: port, Username: "testuser", ConnectTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := cn.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(cn.Shutdown)
	return cn, <-accepted
}

// readFrame 读一个 RIPC 帧（2 字节长度 + 余下）。
func readFrame(c net.Conn) ([]byte, bool) {
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, false
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	b := make([]byte, n)
	copy(b, hdr[:])
	if _, err := io.ReadFull(c, b[2:]); err != nil {
		return nil, false
	}
	_ = c.SetReadDeadline(time.Time{})
	return b, true
}

// connAckFrame 构造服务端 connack（RIPC v14 + key exchange + comp=0）。
func connAckFrame() []byte {
	b := []byte{0, 0, 0x01, 0x01, 10, 0}
	b = append(b, 0, 0, 0, 9) // RIPC_VERSION_14 = 9
	b = append(b, 0x18, 0x00) // maxMsgSize 6144
	b = append(b, 0, 30, 14, 1)
	b = append(b, 0, 0) // comp=0
	b = append(b, 0)    // zlib level
	// key exchange（P=0 → 客户端回 0 长度 key，简化测试）
	b = append(b, 0x08, 1, 24)
	b = append(b, make([]byte, 24)...)
	b = append(b, 1, 0)
	binary.BigEndian.PutUint16(b[0:2], uint16(len(b)))
	return b
}

// writeDataFrame 写一条数据帧。
func writeDataFrame(c net.Conn, payload []byte) {
	frame := make([]byte, 3+len(payload))
	binary.BigEndian.PutUint16(frame, uint16(len(frame)))
	frame[2] = 0x02
	copy(frame[3:], payload)
	_, _ = c.Write(frame)
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Port: 1, Username: "u"}); err == nil {
		t.Fatal("host 为空应报错")
	}
	if _, err := New(Config{Host: "127.0.0.1", Username: "u"}); err == nil {
		t.Fatal("port 非法应报错")
	}
	if _, err := New(Config{Host: "127.0.0.1", Port: 1}); err == nil {
		t.Fatal("username 为空应报错")
	}
}

func TestOpsBeforeConnectFailClosed(t *testing.T) {
	c, err := New(Config{Host: "127.0.0.1", Port: 1, Username: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(); err == nil {
		t.Fatal("未连接 Login 应报错")
	}
	if err := c.Submit("TEST", 999, 3); err == nil {
		t.Fatal("未连接 Submit 应报错")
	}
	if _, err := c.Read(); err == nil {
		t.Fatal("未连接 Read 应报错")
	}
	if c.Ready() {
		t.Fatal("未连接 Ready 应为 false")
	}
	c.Ping()     // 不应 panic
	c.Shutdown() // 幂等
}

func TestLoginAndSubmitBytes(t *testing.T) {
	cn, srv := dialNative(t)
	defer srv.Close()

	if err := cn.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	login, ok := readFrame(srv)
	if !ok {
		t.Fatal("服务端未读到 login 帧")
	}
	const wantLogin = "00150101000000010400800b0608" + "7465737475736572" + "01" // name="testuser"(8)
	if got := hex.EncodeToString(login[3:]); got != wantLogin {
		t.Fatalf("login 帧不对:\n got=%s\nwant=%s", got, wantLogin)
	}

	if err := cn.Submit("TEST", 999, 3); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	item, ok := readFrame(srv)
	if !ok {
		t.Fatal("服务端未读到 item 帧")
	}
	const wantItem = "00140106000000030400800a07fe03e7045445535401"
	if got := hex.EncodeToString(item[3:]); got != wantItem {
		t.Fatalf("item 帧不对:\n got=%s\nwant=%s", got, wantItem)
	}
}

// TestSubmitBatchPackingGolden 批量提交必须打成**一个** packing 帧：
// opcode=0x12，帧内逐条 [u16 len][msg]，两条 item request 的字节与单条 Submit 黄金值一致。
func TestSubmitBatchPackingGolden(t *testing.T) {
	cn, srv := dialNative(t)
	defer srv.Close()

	err := cn.SubmitBatch([]SubmitItem{
		{Item: "AAA", ServiceID: 999, StreamID: 3},
		{Item: "BBB", ServiceID: 999, StreamID: 4},
	})
	if err != nil {
		t.Fatalf("SubmitBatch: %v", err)
	}
	frame, ok := readFrame(srv)
	if !ok {
		t.Fatal("服务端未读到批量帧")
	}
	// total=3+(2+21)+(2+21)=49=0x31；每条 21 字节（name 3 字符 → keySize=9，与 4 字符黄金值差 1）。
	const want = "003112" +
		"0015" + "00130106000000030400800907fe03e70341414101" +
		"0015" + "00130106000000040400800907fe03e70342424201"
	if got := hex.EncodeToString(frame); got != want {
		t.Fatalf("批量 packing 帧不对:\n got=%s\nwant=%s", got, want)
	}
}

// TestSubmitBatchEmptyNoFrame 空批量不得产生任何帧（poll 循环空转时不能刷帧）。
func TestSubmitBatchEmptyNoFrame(t *testing.T) {
	cn, srv := dialNative(t)
	defer srv.Close()
	if err := cn.SubmitBatch(nil); err != nil {
		t.Fatalf("空批量不应报错: %v", err)
	}
	_ = srv.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := srv.Read(make([]byte, 1))
	_ = srv.SetReadDeadline(time.Time{})
	if err == nil {
		t.Fatal("空批量不应产生任何帧")
	}
}

func TestReadMapping(t *testing.T) {
	cn, srv := dialNative(t)
	defer srv.Close()

	// 初始无数据 → Ret=-11。
	res, err := cn.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Ret != RetWouldBlock || res.Data != nil || res.Ping {
		t.Fatalf("空读应为 WOULD_BLOCK，实际 %+v", res)
	}

	// 服务端发 ping（3 字节空帧）→ Ping=true/Ret=-13。
	_, _ = srv.Write([]byte{0x00, 0x03, 0x02})
	res = waitNative(t, cn)
	if !res.Ping || res.Ret != RetReadPing {
		t.Fatalf("应识别 ping，实际 %+v", res)
	}

	// 数据帧 → Data 原样。
	writeDataFrame(srv, []byte{0xAA, 0xBB, 0xCC})
	res = waitNative(t, cn)
	if res.Ret != RetSuccess || string(res.Data) != "\xaa\xbb\xcc" {
		t.Fatalf("数据帧不对: %+v", res)
	}

	// 对端关闭 → Read 返回错误（ErrClosed）。
	_ = srv.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cn.Read(); err != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("对端关闭后 Read 必须报错")
}

func waitNative(t *testing.T, cn *Conn) ReadResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		res, err := cn.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if res.Ret != RetWouldBlock {
			return res
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("等待读超时")
	return ReadResult{}
}

// TestShutdownIdempotent Shutdown 可重复调用，之后操作 fail-closed。
func TestShutdownIdempotent(t *testing.T) {
	cn, srv := dialNative(t)
	defer srv.Close()
	cn.Shutdown()
	cn.Shutdown()
	if cn.Ready() {
		t.Fatal("Shutdown 后 Ready 应为 false")
	}
	if err := cn.Login(); err == nil {
		t.Fatal("Shutdown 后 Login 必须报错")
	} else if errors.Is(err, io.EOF) {
		t.Fatal("错误应是未连接而不是 EOF")
	}
}
