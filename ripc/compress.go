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
//   - DEFLATE 的回溯窗口 ≤ 32KiB，因此用 compress/flate 的 NewReaderDict，
//     以前 32KiB 输出作为 preset dictionary，即可对任意帧独立解码且结果与连续流一致；
//   - 首帧 payload 带 zlib 头（CMF/FLG 两字节），校验后剥掉；后续帧是裸 DEFLATE。
//   - 流未结束（sync flush 边界）时 flate 会返回 io.ErrUnexpectedEOF，这里视为正常。
package ripc

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
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
type zlibStream struct {
	hist    []byte // 最近 32KiB 解压输出（作为下一帧的 preset dictionary）
	started bool   // 首帧已剥掉 zlib 头
}

func (z *zlibStream) Decompress(p []byte) ([]byte, error) {
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
		return nil, nil
	}
	r := flate.NewReaderDict(bytes.NewReader(p), z.hist)
	out, err := io.ReadAll(r)
	_ = r.Close()
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
		// 允许重叠：逐字节复制（同 LZ4_decompress_safe）
		start := len(dst) - offset
		for k := 0; k < matchLen; k++ {
			dst = append(dst, dst[start+k])
		}
	}
	return dst, nil
}
