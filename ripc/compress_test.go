package ripc

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// zlibFrames 用 Go 的 zlib writer（deflateInit 语义 + Flush = Z_SYNC_FLUSH）
// 生成「一个持续流、按帧切段」的压缩字节，模拟 C 服务端的发送方式。
func zlibFrames(t *testing.T, payloads ...[]byte) [][]byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	frames := make([][]byte, 0, len(payloads))
	for _, p := range payloads {
		if _, err := zw.Write(p); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
		if err := zw.Flush(); err != nil {
			t.Fatalf("zlib flush: %v", err)
		}
		frames = append(frames, append([]byte(nil), buf.Bytes()...))
		buf.Reset()
	}
	_ = zw.Close()
	return frames
}

// TestZlibStreamAcrossFrames 跨帧解压：第二帧的回溯引用第一帧输出（preset dictionary）。
func TestZlibStreamAcrossFrames(t *testing.T) {
	a := []byte(strings.Repeat("XAG= bid/ask 42.1234 ", 200))
	b := []byte(strings.Repeat("XAG= bid/ask 42.1234 ", 200) + "tail")
	frames := zlibFrames(t, a, b)
	// 首帧必须带 zlib 头（CMF=0x78 一类）
	if len(frames[0]) < 2 || frames[0][0]&0x0F != 8 {
		t.Fatalf("首帧应带 zlib 头: %x", frames[0][:2])
	}
	if bytes.HasPrefix(frames[1], frames[0][:2]) {
		t.Log("提示：第二帧恰好也以相同两字节开头（不构成失败）")
	}
	z := &zlibStream{}
	got1, err := z.Decompress(frames[0])
	if err != nil {
		t.Fatalf("首帧解压失败: %v", err)
	}
	if !bytes.Equal(got1, a) {
		t.Fatalf("首帧输出不一致: len=%d/%d", len(got1), len(a))
	}
	got2, err := z.Decompress(frames[1])
	if err != nil {
		t.Fatalf("第二帧解压失败: %v", err)
	}
	if !bytes.Equal(got2, b) {
		t.Fatalf("第二帧输出不一致（字典失效会在这里暴露）: len=%d/%d", len(got2), len(b))
	}
}

func TestZlibRejectsBadHeader(t *testing.T) {
	z := &zlibStream{}
	if _, err := z.Decompress([]byte{0x00, 0x00}); err == nil {
		t.Fatal("非法 zlib 头应报错")
	}
	z = &zlibStream{}
	if _, err := z.Decompress([]byte{0x78, 0x00}); err == nil {
		t.Fatal("FCHECK 非法的 zlib 头应报错")
	}
}

// TestLZ4DecodeBlock 覆盖 literal-only、match、重叠复制与非法 offset。
func TestLZ4DecodeBlock(t *testing.T) {
	// literal-only: token=0xF0 + 扩展长度 5 + 20 字节 → 20 字节输出
	lit := []byte("0123456789abcdefghij")
	src := append([]byte{0xF0, 5}, lit...)
	out, err := lz4DecodeBlock(src, 1024)
	if err != nil || !bytes.Equal(out, lit) {
		t.Fatalf("literal-only 解码 = %q, %v", out, err)
	}

	// 短 literal（3 字节）+ match(offset=3, len=6) → "abcabcabc"
	src = []byte{0x32, 'a', 'b', 'c', 0x03, 0x00}
	out, err = lz4DecodeBlock(src, 1024)
	if err != nil || string(out) != "abcabcabc" {
		t.Fatalf("match 解码 = %q, %v（期望 abcabcabc）", out, err)
	}

	// offset=0 非法
	if _, err := lz4DecodeBlock([]byte{0x00, 0x00, 0x00}, 64); err == nil {
		t.Fatal("offset=0 应报错")
	}
	// offset 超过已输出长度
	if _, err := lz4DecodeBlock([]byte{0x10, 'a', 0x05, 0x00}, 64); err == nil {
		t.Fatal("offset 越界应报错")
	}
	// 输出上限
	if _, err := lz4DecodeBlock(append([]byte{0xF0, 5}, lit...), 10); err == nil {
		t.Fatal("超过输出上限应报错")
	}
}

// TestForcedZlibCompression 服务端用 connack flags 0x80 强制 zlib：
// 握手成功，两个压缩帧都能解出原文。
func TestForcedZlibCompression(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second

	p1 := []byte(strings.Repeat("REFRESH payload ", 50))
	p2 := []byte(strings.Repeat("REFRESH payload ", 50) + "UPDATE tail")
	frames := zlibFrames(t, p1, p2)

	ack := connAck(9, 6144, 60, false)
	ack[2] |= compForce // 强制压缩，comp 字段仍为 0 → C 语义取 zlib

	go func() {
		serveHandshake(t, srv, ack)
		for _, f := range frames {
			frame := make([]byte, 3+len(f))
			binary.BigEndian.PutUint16(frame, uint16(len(frame)))
			frame[2] = ipcData | ipcCompData
			copy(frame[3:], f)
			_, _ = srv.Write(frame)
		}
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()
	if c.compression != compZlib {
		t.Fatalf("协商压缩类型 = %d，期望 zlib(1)", c.compression)
	}
	for i, want := range [][]byte{p1, p2} {
		data, ping, err := waitRead(c, 2*time.Second)
		if err != nil || ping {
			t.Fatalf("第 %d 帧读取失败: err=%v ping=%v", i+1, err, ping)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("第 %d 帧解压结果不一致: len=%d/%d", i+1, len(data), len(want))
		}
	}
}

// TestCompressedPacked 压缩帧 + PACKING 叠加：解压后按 [uint16 len] 拆两条。
func TestCompressedPacked(t *testing.T) {
	srv, cli := tcpPair(t)
	defer srv.Close()
	defer cli.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 1
	cfg.Timeout = 2 * time.Second

	m1 := []byte("message-one")
	m2 := []byte("message-two")
	var packed bytes.Buffer
	for _, m := range [][]byte{m1, m2} {
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(m)))
		packed.Write(l[:])
		packed.Write(m)
	}
	frames := zlibFrames(t, packed.Bytes())
	ack := connAck(9, 6144, 60, false)
	ack[2] |= compForce

	go func() {
		serveHandshake(t, srv, ack)
		f := frames[0]
		frame := make([]byte, 3+len(f))
		binary.BigEndian.PutUint16(frame, uint16(len(frame)))
		frame[2] = ipcData | ipcCompData | ipcPacking
		copy(frame[3:], f)
		_, _ = srv.Write(frame)
	}()

	c, err := New(cli, cfg)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer c.Close()
	for i, want := range [][]byte{m1, m2} {
		data, _, err := waitRead(c, 2*time.Second)
		if err != nil || !bytes.Equal(data, want) {
			t.Fatalf("packed 第 %d 条 = %q, err=%v", i+1, data, err)
		}
	}
}
