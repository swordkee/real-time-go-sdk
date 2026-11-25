package ripc

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// 本文件锁住两项性能优化的**逐字节等价性**：
//   - zlib：复用单个 inflate 状态机（Reset + preset dict）vs 每帧 flate.NewReaderDict；
//   - LZ4 ：overlap 的 memmove / 整数倍翻段复制 vs 原逐字节拷贝。
//
// 参考实现（*Ref）刻意保留优化前的写法，任何语义漂移都会在这里变红。

// ---------------------------------------------------------------- 参考实现

// lz4DecodeBlockByteCopyRef 是优化前的 LZ4 解码（match 逐字节拷贝），仅作测试/基准对照。
func lz4DecodeBlockByteCopyRef(src []byte, maxOut int) ([]byte, error) {
	if maxOut <= 0 {
		maxOut = 64 << 20
	}
	dst := make([]byte, 0, maxOut)
	i := 0
	for i < len(src) {
		token := src[i]
		i++
		litLen := int(token >> 4)
		if litLen == 15 {
			for {
				if i >= len(src) {
					return nil, errors.New("lz4: literal 长度截断")
				}
				b := src[i]
				i++
				litLen += int(b)
				if b != 255 {
					break
				}
			}
		}
		if litLen > 0 {
			if i+litLen > len(src) {
				return nil, errors.New("lz4: literal 越界")
			}
			if len(dst)+litLen > maxOut {
				return nil, errors.New("lz4: 输出超过上限")
			}
			dst = append(dst, src[i:i+litLen]...)
			i += litLen
		}
		if i == len(src) {
			break
		}
		if i+2 > len(src) {
			return nil, errors.New("lz4: match offset 截断")
		}
		offset := int(src[i]) | int(src[i+1])<<8
		i += 2
		if offset == 0 || offset > len(dst) {
			return nil, fmt.Errorf("lz4: 非法 offset %d（已输出 %d）", offset, len(dst))
		}
		matchLen := int(token&0x0F) + 4
		if token&0x0F == 15 {
			for {
				if i >= len(src) {
					return nil, errors.New("lz4: match 长度截断")
				}
				b := src[i]
				i++
				matchLen += int(b)
				if b != 255 {
					break
				}
			}
		}
		if len(dst)+matchLen > maxOut {
			return nil, errors.New("lz4: match 输出超过上限")
		}
		start := len(dst) - offset
		for k := 0; k < matchLen; k++ {
			dst = append(dst, dst[start+k])
		}
	}
	return dst, nil
}

// zlibStreamPerFrameRef 是优化前的 zlib 解压器（每帧新建 flate reader）。
type zlibStreamPerFrameRef struct {
	hist    []byte
	started bool
}

func (z *zlibStreamPerFrameRef) Decompress(p []byte) ([]byte, error) {
	if !z.started {
		if len(p) < 2 {
			return nil, errors.New("zlib: 首帧缺 zlib 头")
		}
		cmf, flg := p[0], p[1]
		if cmf&0x0F != 8 {
			return nil, fmt.Errorf("zlib: 不支持的方法 CM=%d", cmf&0x0F)
		}
		if (uint16(cmf)<<8|uint16(flg))%31 != 0 {
			return nil, errors.New("zlib: 头校验失败")
		}
		if flg&0x20 != 0 {
			return nil, errors.New("zlib: 不支持 FDICT 预置字典")
		}
		p = p[2:]
		z.started = true
	}
	if len(p) == 0 {
		return nil, nil
	}
	r := flate.NewReaderDict(bytes.NewReader(p), z.hist)
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("zlib: %w", err)
	}
	z.hist = appendHistory(z.hist, out)
	return out, nil
}

// ---------------------------------------------------------------- LZ4 等价

// 显式边界用例：offset 与 matchLen 的相对关系覆盖三种分支
// （offset>matchLen / offset==matchLen / offset<matchLen，含 offset=1 的 RLE）。
func TestLZ4OverlapEquivalenceBoundary(t *testing.T) {
	lit := []byte("0123456789abcdefghij") // 20 字节
	cases := []struct {
		name string
		src  []byte
		max  int
	}{
		{"offset=1,matchLen=20（纯 RLE）", lz4LitOnlyWithMatchTail([]byte("a"), 1, 20), 256},
		{"offset=2,matchLen=8", lz4LitOnlyWithMatchTail([]byte("ab"), 2, 8), 256},
		{"offset=3,matchLen=6（原有用例）", lz4LitOnlyWithMatchTail([]byte("abc"), 3, 6), 256},
		{"offset==matchLen=11", lz4LitOnlyWithMatchTail([]byte("abcdefghijk"), 11, 11), 256},
		{"offset>matchLen（memmove 快路径）", lz4LitOnlyWithMatchTail(lit, 5, 4), 256},
		{"offset==matchLen+1", lz4LitOnlyWithMatchTail(lit, 12, 11), 256},
		{"offset 小且 matchLen 跨多段（offset=4,len=41）", lz4LitOnlyWithMatchTail([]byte("wxyz"), 4, 41), 256},
		{"连续两条 match（重叠+不重叠）",
			append(lz4LitOnlyWithMatchTail([]byte("abc"), 2, 30), lz4LitOnlyWithMatchTail(lit, 18, 4)...), 512},
		{"offset=0 fail-closed", []byte{0x10, 'a', 0x00, 0x00}, 64},
		{"offset>已输出 fail-closed", []byte{0x10, 'a', 0x05, 0x00}, 64},
		{"输出上限 fail-closed（重叠路径）", lz4LitOnlyWithMatchTail([]byte("a"), 1, 20), 20},
		{"literal-only", append([]byte{0xF0, 5}, lit...), 64},
	}
	if len(cases) < 8 {
		t.Fatalf("边界用例不足 8 组")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, gotErr := lz4DecodeBlock(c.src, c.max)
			want, wantErr := lz4DecodeBlockByteCopyRef(c.src, c.max)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("错误行为不一致: got=%v want=%v", gotErr, wantErr)
			}
			if gotErr == nil && !bytes.Equal(got, want) {
				t.Fatalf("输出不一致: got=%q want=%q", got, want)
			}
			if gotErr != nil && gotErr.Error() != wantErr.Error() {
				t.Fatalf("错误文本不一致: got=%v want=%v", gotErr, wantErr)
			}
		})
	}
}

// 随机等价：由生成器构造**合法** LZ4 block（literal + 合法 offset/match），
// 覆盖 offset 与 matchLen 的各种组合与链式 match。
func TestLZ4OverlapEquivalenceRandom(t *testing.T) {
	r := rand.New(rand.NewSource(20261125))
	for iter := 0; iter < 500; iter++ {
		budget := 1 + r.Intn(4096)
		src := genLZ4ValidBlock(r, budget)
		maxOut := budget
		got, gotErr := lz4DecodeBlock(src, maxOut)
		want, wantErr := lz4DecodeBlockByteCopyRef(src, maxOut)
		if gotErr != nil || wantErr != nil {
			t.Fatalf("iter=%d 生成器产出了非法 block: got=%v want=%v", iter, gotErr, wantErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("iter=%d 输出不一致: len=%d/%d", iter, len(got), len(want))
		}
		if len(got) != budget {
			t.Fatalf("iter=%d 输出长度 %d != budget %d", iter, len(got), budget)
		}
	}
}

// 大 offset（>255）与长 match（扩展长度）组合，验证 u16 offset 与 >15 长度路径。
func TestLZ4OverlapEquivalenceLargeOffset(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for _, offset := range []int{256, 257, 1000, 4095, 32768, 65535} {
		base := make([]byte, offset)
		for i := range base {
			base[i] = byte('A' + r.Intn(26))
		}
		matchLen := 4 + r.Intn(80)
		src := lz4LitOnlyWithMatchTail(base, offset, matchLen)
		got, err := lz4DecodeBlock(src, offset+matchLen)
		if err != nil {
			t.Fatalf("offset=%d 解码失败: %v", offset, err)
		}
		want, _ := lz4DecodeBlockByteCopyRef(src, offset+matchLen)
		if !bytes.Equal(got, want) {
			t.Fatalf("offset=%d 大位移输出不一致", offset)
		}
	}
}

// genLZ4ValidBlock 生成一个合法的 raw LZ4 block，解压后恰为 budget 字节。
//
// 遵循 LZ4 block 规范：block 必须以「literal-only 序列」结尾（没有 offset 字段），
// 否则解码器会把后续字节当 offset 读——上一版生成器就在这里产出过 token=0x00 +
// offset=0 的非法输入。字节序为 token → literal 长度扩展 → literals → offset(2) →
// match 长度扩展。
func genLZ4ValidBlock(r *rand.Rand, budget int) []byte {
	var src []byte
	outLen := 0
	for outLen < budget {
		remaining := budget - outLen
		// 收尾：剩余 ≤ 8 或随机决定时，用 literal-only 序列吃掉全部剩余。
		if remaining <= 8 || r.Intn(4) == 0 {
			src = appendGroup(src, randomLits(r, remaining), 0, 0)
			outLen = budget
			break
		}
		litLen := 1 + r.Intn(minInt(48, remaining-4))
		lits := randomLits(r, litLen)
		if remaining-litLen < 5 {
			src = appendGroup(src, randomLits(r, remaining), 0, 0)
			outLen = budget
			break
		}
		// 至少留 1 字节给结尾的 literal-only 序列。
		maxMatch := remaining - litLen - 1
		if maxMatch > 400 {
			maxMatch = 400
		}
		matchLen := 4 + r.Intn(maxMatch-3)
		maxOff := outLen + litLen
		if maxOff > 65535 {
			maxOff = 65535
		}
		offset := 1 + r.Intn(maxOff)
		src = appendGroup(src, lits, offset, matchLen)
		outLen += litLen + matchLen
	}
	return src
}

func randomLits(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + r.Intn(4))
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// appendGroup 按 LZ4 字节序拼一个序列；matchLen==0 表示 literal-only（不写 offset）。
func appendGroup(dst, lits []byte, offset, matchLen int) []byte {
	var token byte
	if len(lits) >= 15 {
		token = 0xF0
	} else {
		token = byte(len(lits)) << 4
	}
	mv := matchLen - 4
	if matchLen >= 4 {
		if mv >= 15 {
			token |= 0x0F
		} else {
			token |= byte(mv)
		}
	}
	dst = append(dst, token)
	if len(lits) >= 15 {
		dst = appendLenExt(dst, len(lits)-15)
	}
	dst = append(dst, lits...)
	if matchLen < 4 {
		return dst
	}
	dst = append(dst, byte(offset&0xFF), byte(offset>>8))
	if mv >= 15 {
		dst = appendLenExt(dst, mv-15)
	}
	return dst
}

// appendLenExt 追加 LZ4 长度扩展字节（255 连续 + 余数）。
func appendLenExt(dst []byte, v int) []byte {
	for v >= 255 {
		dst = append(dst, 255)
		v -= 255
	}
	return append(dst, byte(v))
}

// lz4LitOnlyWithMatchTail 构造「lits 作 literal 段 + 一条 offset/matchLen 的 match」，
// 按 LZ4 字节序拼接（match 长度扩展在 offset 之后）。
func lz4LitOnlyWithMatchTail(lits []byte, offset, matchLen int) []byte {
	var token byte
	if len(lits) >= 15 {
		token = 0xF0
	} else {
		token = byte(len(lits)) << 4
	}
	mv := matchLen - 4
	if mv >= 15 {
		token |= 0x0F
	} else {
		token |= byte(mv)
	}
	out := []byte{token}
	if len(lits) >= 15 {
		out = appendLenExt(out, len(lits)-15)
	}
	out = append(out, lits...)
	out = append(out, byte(offset&0xFF), byte(offset>>8))
	if mv >= 15 {
		out = appendLenExt(out, mv-15)
	}
	return out
}

// ---------------------------------------------------------------- zlib 等价

// 跨多帧、且历史窗口滚动超过 32KiB（第二段起走 appendHistory 的裁剪分支），
// 复用状态机与每帧新建 reader 必须逐字节一致。
func TestZlibReuseMatchesPerFrameRef(t *testing.T) {
	payloads := [][]byte{
		[]byte(strings.Repeat("XAG= bid/ask 42.1234 ", 200)),
		[]byte(strings.Repeat("XAG= bid/ask 42.1234 ", 200) + "tail"),
		[]byte(strings.Repeat("UPDATE XAU= 1900.55 | ", 3000)), // ≈ 60KB，滚动窗口
		[]byte(strings.Repeat("XAU= 1900.55 | ", 100)),
		[]byte(strings.Repeat("INSERT XPT= 980.10 | ", 2500)),
		bytes.Repeat([]byte{0x00, 0xFF, 0x7F}, 8000),
	}
	frames := zlibFrames(t, payloads...)
	if len(frames) != len(payloads) {
		t.Fatalf("帧数 %d != payload 数 %d", len(frames), len(payloads))
	}

	opt := &zlibStream{}
	ref := &zlibStreamPerFrameRef{}
	for i := range frames {
		got, gotErr := opt.Decompress(frames[i])
		want, wantErr := ref.Decompress(frames[i])
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("第 %d 帧错误行为不一致: got=%v want=%v", i, gotErr, wantErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("第 %d 帧输出不一致: len=%d/%d", i, len(got), len(want))
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Fatalf("第 %d 帧与原文不一致: len=%d/%d", i, len(got), len(payloads[i]))
		}
	}
	if len(opt.hist) > maxDict {
		t.Fatalf("历史窗口 %d 超过 maxDict", len(opt.hist))
	}
}

// 同一实例上重复 Reset 不应累积状态：同一帧重复解压结果恒等于原文。
func TestZlibReuseIdempotentFrames(t *testing.T) {
	p := []byte(strings.Repeat("RTTR 1234.5 ", 500))
	frames := zlibFrames(t, p)
	z := &zlibStream{}
	first, err := z.Decompress(frames[0])
	if err != nil {
		t.Fatalf("首帧: %v", err)
	}
	if !bytes.Equal(first, p) {
		t.Fatalf("首帧输出与原文不一致")
	}
	// 另一实例解同一帧：payload 相同、初始状态相同 ⇒ 输出必须一致。
	z2 := &zlibStream{}
	again, err := z2.Decompress(frames[0])
	if err != nil {
		t.Fatalf("重复解压: %v", err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("同一帧两次解压结果不一致")
	}
	// 同一实例连续两次喂同一帧（dict 随之推进）：结果仍需与「每帧新建」参考实现一致。
	z3 := &zlibStream{}
	ref := &zlibStreamPerFrameRef{}
	for i := 0; i < 2; i++ {
		g, gErr := z3.Decompress(frames[0])
		w, wErr := ref.Decompress(frames[0])
		if (gErr == nil) != (wErr == nil) {
			t.Fatalf("第 %d 次重放错误行为不一致: %v / %v", i, gErr, wErr)
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("第 %d 次重放输出与参考实现不一致", i)
		}
	}
}

// 坏数据在两套实现下错误行为一致（错误文本相同）。
func TestZlibReuseErrorParity(t *testing.T) {
	bad := [][]byte{
		{0x78, 0x9C, 0xFF, 0xFF, 0xFF},       // 合法的 zlib 头 + 垃圾 deflate
		{0x78, 0x9C, 0x03, 0x00},             // 空 deflate 流
		{0x78, 0x9C, 0x05, 0x00, 0x01, 0x00}, // reserved block type
	}
	for i, b := range bad {
		got, gotErr := (&zlibStream{}).Decompress(b)
		want, wantErr := (&zlibStreamPerFrameRef{}).Decompress(b)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("第 %d 组错误行为不一致: got=%v want=%v", i, gotErr, wantErr)
		}
		if gotErr != nil && gotErr.Error() != wantErr.Error() {
			t.Fatalf("第 %d 组错误文本不一致: got=%v want=%v", i, gotErr, wantErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("第 %d 组部分输出不一致", i)
		}
	}
}

// 并发调用不能有数据竞争（-race 下有效）。解码本身有跨帧状态，因此每个
// goroutine 用**独立流**串行解自己的帧序列（真实拓扑：每连接一个解压器）。
func TestZlibReuseConcurrentNoRace(t *testing.T) {
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(seed int) {
			defer func() { done <- struct{}{} }()
			payloads := [][]byte{
				[]byte(strings.Repeat("CONCURRENT payload ", 300+seed)),
				[]byte(strings.Repeat("second frame ", 200+seed)),
			}
			frames := zlibFrames(t, payloads...)
			z := &zlibStream{}
			for k := 0; k < 20; k++ {
				for fi, want := range payloads {
					got, err := z.Decompress(frames[fi])
					if err != nil {
						t.Errorf("并发解压失败: %v", err)
						return
					}
					if !bytes.Equal(got, want) {
						t.Errorf("并发解压输出不一致")
						return
					}
				}
				// 一个帧序列跑完即换新流（同一帧不能在同一流上重放：dict 已推进）。
				z = &zlibStream{}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}

// 同一实例被并发调用时锁必须生效：不 panic、不崩、不出数据竞争
// （帧序由调用方保证，这里只验互斥不验语义）。
func TestZlibReuseSameInstanceSerialized(t *testing.T) {
	p := []byte(strings.Repeat("LOCKED payload ", 100))
	frames := zlibFrames(t, p)
	z := &zlibStream{}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for k := 0; k < 50; k++ {
				_, _ = z.Decompress(frames[0]) // 结果可能因 dict 推进而报错，仅验不崩
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
