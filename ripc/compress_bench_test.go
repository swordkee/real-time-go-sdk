package ripc

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math/rand"
	"testing"
)

// 基准对齐审计口径：zlib 帧 ≈45.8KB（本 SDK 行情快照帧的量级）；
// LZ4 match 覆盖 32B / 128B / 512B 三档。
// 每个基准都配一个「优化前」对照，便于看实际收益与变异自检。

// zlibSnapshotPayload 造一段「行情快照」文本：固定 RIC 骨架 + 随机值填充，
// 压缩比约 2:1，代表高熵帧（最坏情形：解码全是 literal）。
func zlibSnapshotPayload(size int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	p := make([]byte, 0, size+128)
	for n := 0; len(p) < size; n++ {
		p = append(p, []byte(fmt.Sprintf("RIC%04d=%.4f/%.4f V%07d L%05d U%06d; ",
			n%1500, r.Float64()*1000, r.Float64()*1000, r.Intn(1000000), r.Intn(100000), r.Intn(1000000)))...)
	}
	return p[:size]
}

// zlibSteadyFrame 产出「持续流里的第二帧」，形状贴近真实行情刷新：
// 第一帧是完整快照，第二帧与第一帧绝大多数字节相同（只有少量字段变化），
// 因此第二帧的压缩数据几乎全是对第一帧的回溯引用 —— 解压必须靠 preset dict，
// 且成本集中在「拷贝」而不是「热切 Huffman 表」，正是本优化的目标路径。
//
// 返回 (裸 DEFLATE payload = 第二帧, hist = 第一帧输出末尾 32KiB)。
func zlibSteadyFrame(size int) (payload, hist []byte) {
	base := zlibSnapshotPayload(size, 42)
	// 第二帧：在 base 上随机改 ~3% 的字节（模拟价格/量变化）。
	next := append([]byte(nil), base...)
	r := rand.New(rand.NewSource(43))
	for i := 0; i < len(next)/32; i++ {
		pos := r.Intn(len(next))
		next[pos] = byte('0' + r.Intn(10))
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(base); err != nil {
		panic(err)
	}
	if err := zw.Flush(); err != nil {
		panic(err)
	}
	buf.Reset() // 丢掉含 zlib 头的首帧字节，只留第二帧
	if _, err := zw.Write(next); err != nil {
		panic(err)
	}
	if err := zw.Flush(); err != nil {
		panic(err)
	}
	_ = zw.Close()
	hist = base
	if len(hist) > maxDict {
		hist = hist[len(hist)-maxDict:]
	}
	return append([]byte(nil), buf.Bytes()...), append([]byte(nil), hist...)
}

const benchZlibSize = 45*1024 + 819 // ≈45.8KB
const benchLZ4Block = 45 * 1024

// BenchmarkZlibDecompressReuse 复用 inflate 状态机（优化后）：每帧 Reset + preset dict。
func BenchmarkZlibDecompressReuse(b *testing.B) {
	payload, hist := zlibSteadyFrame(benchZlibSize)
	b.SetBytes(benchZlibSize)
	b.ReportAllocs()
	z := &zlibStream{started: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		z.hist = hist
		if _, err := z.Decompress(payload); err != nil {
			b.Fatalf("解压失败: %v", err)
		}
	}
}

// BenchmarkZlibDecompressPerFrame 每帧新建 flate reader（优化前基线）。
func BenchmarkZlibDecompressPerFrame(b *testing.B) {
	payload, hist := zlibSteadyFrame(benchZlibSize)
	b.SetBytes(benchZlibSize)
	b.ReportAllocs()
	ref := &zlibStreamPerFrameRef{started: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref.hist = hist
		if _, err := ref.Decompress(payload); err != nil {
			b.Fatalf("解压失败: %v", err)
		}
	}
}

// 小帧（2KB 级）：建流器的固定开销（Huffman 表 + 32KiB 字典缓冲）占主导，
// 这里才是复用收益最接近审计值（≈3×）的形状。
const benchZlibSmall = 2 * 1024

func BenchmarkZlibDecompressSmallReuse(b *testing.B) {
	payload, hist := zlibSteadyFrame(benchZlibSmall)
	b.SetBytes(benchZlibSmall)
	b.ReportAllocs()
	z := &zlibStream{started: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		z.hist = hist
		if _, err := z.Decompress(payload); err != nil {
			b.Fatalf("解压失败: %v", err)
		}
	}
}

func BenchmarkZlibDecompressSmallPerFrame(b *testing.B) {
	payload, hist := zlibSteadyFrame(benchZlibSmall)
	b.SetBytes(benchZlibSmall)
	b.ReportAllocs()
	ref := &zlibStreamPerFrameRef{started: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref.hist = hist
		if _, err := ref.Decompress(payload); err != nil {
			b.Fatalf("解压失败: %v", err)
		}
	}
}

// 低熵帧（同一行文本重复）：解压几乎全是长 match 拷贝，压缩比可达数十倍。
// 这是审计里「45.8KB 帧 → 4.3–7.5µs」那一档的形状（建流器固定开销 + 字典
// 分配占比最高），留作与审计数字对拍的参考。
func zlibLowEntropyFrame(payloadSize int) (payload, hist []byte) {
	line := []byte("XAG= 42.1234 / 42.1236 | XAU= 1900.55 | XPT= 980.10 | ")
	first := bytes.Repeat(line, payloadSize/len(line)+1)[:payloadSize]
	next := append([]byte(nil), first...)
	copy(next[len(next)-64:], []byte("TAIL                         XAG= 42.9999 / 43.0001 | XAU= 1900.99 |"))
	// Z_SYNC_FLUSH 结尾（第二帧的压缩字节）
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(first); err != nil {
		panic(err)
	}
	if err := zw.Flush(); err != nil {
		panic(err)
	}
	buf.Reset()
	if _, err := zw.Write(next); err != nil {
		panic(err)
	}
	if err := zw.Flush(); err != nil {
		panic(err)
	}
	_ = zw.Close()
	hist = first
	if len(hist) > maxDict {
		hist = hist[len(hist)-maxDict:]
	}
	return append([]byte(nil), buf.Bytes()...), append([]byte(nil), hist...)
}

func BenchmarkZlibDecompressLowEntropyReuse(b *testing.B) {
	payload, hist := zlibLowEntropyFrame(benchZlibSize)
	b.SetBytes(benchZlibSize)
	b.ReportAllocs()
	z := &zlibStream{started: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		z.hist = hist
		if _, err := z.Decompress(payload); err != nil {
			b.Fatalf("解压失败: %v", err)
		}
	}
}

func BenchmarkZlibDecompressLowEntropyPerFrame(b *testing.B) {
	payload, hist := zlibLowEntropyFrame(benchZlibSize)
	b.SetBytes(benchZlibSize)
	b.ReportAllocs()
	ref := &zlibStreamPerFrameRef{started: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref.hist = hist
		if _, err := ref.Decompress(payload); err != nil {
			b.Fatalf("解压失败: %v", err)
		}
	}
}

// lz4BenchSrc 构造「literal 前缀 = offset 长 + 单条 match(offset, matchLen)」的合法
// block。前缀取最小（=offset），让基准测的是 match 拷贝本身而不是 literal 拷贝。
func lz4BenchSrc(prefixLen, offset, matchLen int) []byte {
	if prefixLen < offset {
		prefixLen = offset
	}
	base := make([]byte, prefixLen)
	r := rand.New(rand.NewSource(1))
	for i := range base {
		base[i] = byte('a' + r.Intn(26))
	}
	return lz4LitOnlyWithMatchTail(base, offset, matchLen)
}

// BenchmarkLZ4MatchOverlap 优化后（offset>=len 用 memmove，重叠用翻段）。
// offset=64 让 32B 走 memmove 快路径、128B/512B 走翻段；rle 组固定 offset=1。
func BenchmarkLZ4MatchOverlap(b *testing.B) {
	run := func(name string, src []byte, max int, fn func([]byte, int) ([]byte, error)) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := fn(src, max); err != nil {
					b.Fatalf("解码失败: %v", err)
				}
			}
		})
	}
	for _, tc := range []struct{ offset, matchLen int }{
		{64, 32}, {64, 128}, {64, 512},
	} {
		src := lz4BenchSrc(tc.offset, tc.offset, tc.matchLen)
		max := tc.offset + tc.matchLen
		run(fmt.Sprintf("match%d_offset%d", tc.matchLen, tc.offset), src, max, lz4DecodeBlock)
	}
	for _, matchLen := range []int{32, 128, 512} {
		src := lz4BenchSrc(1, 1, matchLen)
		max := 1 + matchLen
		run(fmt.Sprintf("rle%d", matchLen), src, max, lz4DecodeBlock)
	}
}

// BenchmarkLZ4MatchByteCopyRef 逐字节基线（优化前）。
func BenchmarkLZ4MatchByteCopyRef(b *testing.B) {
	run := func(name string, src []byte, max int) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := lz4DecodeBlockByteCopyRef(src, max); err != nil {
					b.Fatalf("解码失败: %v", err)
				}
			}
		})
	}
	for _, tc := range []struct{ offset, matchLen int }{
		{64, 32}, {64, 128}, {64, 512},
	} {
		src := lz4BenchSrc(tc.offset, tc.offset, tc.matchLen)
		run(fmt.Sprintf("match%d_offset%d", tc.matchLen, tc.offset), src, tc.offset+tc.matchLen)
	}
	for _, matchLen := range []int{32, 128, 512} {
		src := lz4BenchSrc(1, 1, matchLen)
		run(fmt.Sprintf("rle%d", matchLen), src, 1+matchLen)
	}
}

// BenchmarkLZ4DecodeFullBlock 整块解码（literal/match 混合），端到端口径。
func BenchmarkLZ4DecodeFullBlock(b *testing.B) {
	r := rand.New(rand.NewSource(20261125))
	src := genLZ4ValidBlock(r, benchLZ4Block)
	b.SetBytes(benchLZ4Block)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := lz4DecodeBlock(src, benchLZ4Block); err != nil {
			b.Fatalf("解码失败: %v", err)
		}
	}
}
