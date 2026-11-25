// 本文件是 RIPC 数据帧压缩的纯 Go 实现，逐条对照 RTSDK C：
//
//   - zlib：Impl/Transport/ripccomp.c（deflateInit/inflateInit + Z_SYNC_FLUSH，
//     一个连接上**一个持续流**，每帧以 sync flush 结束）
//   - LZ4 ：同一文件（LZ4_decompress_safe = raw LZ4 block 格式）
//   - 帧语义：Impl/Transport/rsslSocketTransportImpl.c ipcReadSession()（IPC_COMP_DATA /
//     IPC_COMP_FRAG；压缩帧的 RIPC 头不入压缩流，只有 payload 被压缩）
//
// zlib 的处理方式（不用 goroutine、不用整段重放）：
//   - 每个压缩帧的 payload 是「同一个 raw DEFLATE 流的一段」，且以 Z_SYNC_FLUSH 结束；
//   - DEFLATE 的回溯窗口 ≤ 32KiB，因此以前 32KiB 输出作为 preset dictionary
//     （flate.Reader.Reset(r, dict)），即可对任意帧独立解码且结果与连续流一致；
//     解压状态机本身在连接生命周期内只建一次并逐帧重置（见 zlibStream）。
//   - 首帧 payload 带 zlib 头（CMF/FLG 两字节），校验后剥掉；后续帧是裸 DEFLATE。
//   - 流未结束（sync flush 边界）时 flate 会返回 io.ErrUnexpectedEOF，这里视为正常。
package ripc

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"sync"
)

// 压缩类型（ripc_int.h ripccompressions: NONE=0 / ZLIB=1 / LZ4=2）。
const (
	compNone = 0
	compZlib = 1
	compLZ4  = 2
	// compForce 是 **connack 帧 flags 字节（buf[2]）** 的 0x80：服务端强制压缩。
	// (ripc_int.h RSSL_COMP_FORCE；客户端见 ipcWaitAck: if ((flags & RSSL_COMP_FORCE) && (!comp)) comp = RSSL_COMP_ZLIB)
	compForce = 0x80
)

// maxDict 是 DEFLATE 的最大回溯窗口。
const maxDict = 32 * 1024

// decompressor 解一个压缩帧的 payload（返回解压后的字节）。
type decompressor interface {
	Decompress(p []byte) ([]byte, error)
}

// newDecompressor 按协商类型建解压器（maxMsg 用于 LZ4 的输出上限，与 C 的
// maxMsgSize/decompressBuf 容量一致）。
func newDecompressor(typ byte, maxMsg int) decompressor {
	switch typ {
	case compZlib:
		return &zlibStream{}
	case compLZ4:
		return &lz4Stream{max: maxMsg}
	default:
		return nil
	}
}

// zlibStream 维护跨帧的 32KiB 历史窗口。
//
// 复用一个 inflate 状态机（flate.Resetter）而不是每帧 flate.NewReaderDict：
// NewReaderDict 每帧都重新分配 Huffman 表、bits/codebits 与 32KiB 字典缓冲，
// 在该 SDK 的帧率下占解压成本的大头；Reset 只重绑 reader 与 preset dict。
// 这也更贴近本文件开头描述的对端语义——「一个连接上一个持续流」，解压侧因此
// 同样每连接只建一个流，逐帧 reset + 重设字典 + 独立结束，而不是逐帧重建流。
//
// 不可值拷贝（含 bytes.Reader / flate 状态机）。Decompress 是唯一入口，
// 生产调用方只有 Conn.readLoop 一个 goroutine，但对外仍加锁保证并发安全。
type zlibStream struct {
	mu      sync.Mutex
	hist    []byte // 最近 32KiB 解压输出（作为下一帧的 preset dictionary）
	started bool   // 首帧已剥掉 zlib 头

	src bytes.Reader  // 每帧 Reset 到当前 payload，避免每帧新建 reader
	fl  io.ReadCloser // 复用的 inflate 状态机（同时实现 flate.Resetter）
	rs  flate.Resetter
}

func (z *zlibStream) Decompress(p []byte) ([]byte, error) {
	z.mu.Lock()
	defer z.mu.Unlock()

	if !z.started {
		if len(p) < 2 {
			return nil, errors.New("zlib: 首帧缺 zlib 头")
		}
		cmf, flg := p[0], p[1]
		if cmf&0x0F != 8 { // CM 必须是 deflate
			return nil, fmt.Errorf("zlib: 不支持的方法 CM=%d", cmf&0x0F)
		}
		if (uint16(cmf)<<8|uint16(flg))%31 != 0 { // FCHECK
			return nil, errors.New("zlib: 头校验失败")
		}
		if flg&0x20 != 0 { // FDICT：C 的 deflateInit 不会设预置字典
			return nil, errors.New("zlib: 不支持 FDICT 预置字典")
		}
		p = p[2:]
		z.started = true
	}
	if len(p) == 0 {
		// 空 payload：不推进解压状态机（与旧实现一致，旧实现也不会建 reader）。
		return nil, nil
	}
	z.src.Reset(p)
	if z.fl == nil {
		r := flate.NewReaderDict(&z.src, z.hist)
		rs, ok := r.(flate.Resetter)
		if !ok { // 标准库实现恒满足；fail-closed 防未来语义漂移
			_ = r.Close()
			return nil, errors.New("zlib: inflate reader 不支持 Reset")
		}
		z.fl, z.rs = r, rs
	} else if err := z.rs.Reset(&z.src, z.hist); err != nil { // preset dict 每帧重设
		return nil, fmt.Errorf("zlib: 重置解压器失败: %w", err)
	}
	// 每帧照旧 Close（与旧实现同形状）：flate 的 Close 只回放读过程中已记录的错误
	// （EOF 归零），不释放也不锁死状态机，故不影响后续 Reset 复用。
	out, err := io.ReadAll(z.fl)
	_ = z.fl.Close()
	// sync flush 边界：流还有后续帧，flate 会在读下一块时遇到 EOF ⇒ ErrUnexpectedEOF。
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("zlib: %w", err)
	}
	z.hist = appendHistory(z.hist, out)
	return out, nil
}

// appendHistory 把本次输出并入历史，只保留最后 maxDict 字节。
func appendHistory(h, out []byte) []byte {
	if len(out) >= maxDict {
		return append([]byte(nil), out[len(out)-maxDict:]...)
	}
	h = append(h, out...)
	if len(h) > maxDict {
		h = append([]byte(nil), h[len(h)-maxDict:]...)
	}
	return h
}

// lz4Stream 解 raw LZ4 block（LZ4_decompress_safe 语义，输出上限 max）。
type lz4Stream struct {
	max int
}

func (l *lz4Stream) Decompress(p []byte) ([]byte, error) {
	return lz4DecodeBlock(p, l.max)
}

// lz4DecodeBlock 对照 LZ4_decompress_safe：
//
//	token: 高 4 位=literal 长度，低 4 位=match 长度-4（15 表示后面跟扩展字节，累加至 !=255）
//	literals → [offset u16 LE] → match（offset 可重叠复制）
//	块末尾只允许 literal（没有 offset）。
func lz4DecodeBlock(src []byte, maxOut int) ([]byte, error) {
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
			// 最后一组 literal 之后没有 match
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
		oldLen := len(dst)
		resliceTo(&dst, oldLen+matchLen)
		lz4CopyMatch(dst, oldLen, offset, matchLen)
	}
	return dst, nil
}

// lz4CopyMatch 把 match 展开写入 dst[oldLen:oldLen+matchLen]（调用方已保证长度）。
//
// 原语义（LZ4_decompress_safe 的 forward copy）= 逐字节
// dst[oldLen+k] = dst[oldLen-offset+k]，k ∈ [0,matchLen)。由该定义可推出两条逐字节等价的快路径：
//   - offset >= matchLen：源区间 [oldLen-offset, oldLen-offset+matchLen) 整个落在已输出区内，
//     展开后无重叠 ⇒ 一次 memmove（Go 的 copy 落到 runtime.memmove）等价。
//   - offset <  matchLen：结果必是「最后 offset 字节」的整数倍重复，按已复制长度 n 翻倍复制
//     （n=offset,2·offset,4·offset…）。每次 copy 的源 dst[oldLen,oldLen+n) 与目的
//     dst[oldLen+n,…) 首尾相接、不重叠，因此与逐字节展开逐字节相同。
//
// offset==0 / offset>oldLen 已在调用方 fail-closed；这里 start ∈ [0,oldLen)。
func lz4CopyMatch(dst []byte, oldLen, offset, matchLen int) {
	start := oldLen - offset
	out := dst[oldLen : oldLen+matchLen]
	if offset >= matchLen {
		copy(out, dst[start:start+matchLen])
		return
	}
	n := copy(out, dst[start:start+offset])
	for n < matchLen {
		n += copy(out[n:], out[:n])
	}
}

// resliceTo 把 dst 的长度扩到 n，容量不足时重新分配（保内容）。
// lz4DecodeBlock 的 dst 目前恒以 cap=maxOut 分配，此兜底只为防御未来改动。
func resliceTo(dst *[]byte, n int) {
	if cap(*dst) >= n {
		*dst = (*dst)[:n]
		return
	}
	nd := make([]byte, len(*dst), n)
	copy(nd, *dst)
	*dst = nd
}
