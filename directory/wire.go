package directory

import (
	"fmt"

	"github.com/swordkee/real-time-go-sdk/rwf"
)

// reader 是 RWF 报文上的大端游标。原语逐条对照 LSEG ETA C：
//   - Impl/Util/Include/rtr/rwfNetwork.h  rwfGet8/16/32、rwfGetResBitU15、rwfGetBuffer16
//   - Impl/Util/Include/rtr/rwfNet.h:3963 RWF_GET_RESBIT_U30（map totalCountHint）
//   - _rsslDecodeBuffer15（resBitU15 长度前缀），见 Codec/mapDecoder.c:163 的调用点
//
// 本包不修改 rwf：rwf 的 reader 未导出，这里复刻同一组原语，错误统一用 rwf 的哨兵
// （errors.Is(err, rwf.ErrIncompleteData) 等仍成立）。
type reader struct {
	b   []byte
	pos int
}

func (r *reader) remaining() int { return len(r.b) - r.pos }

func (r *reader) need(n int) error {
	if n < 0 || n > r.remaining() {
		return fmt.Errorf("%w: 需要 %d 字节，剩余 %d", rwf.ErrIncompleteData, n, r.remaining())
	}
	return nil
}

func (r *reader) u8() (uint8, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *reader) u16() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := uint16(r.b[r.pos])<<8 | uint16(r.b[r.pos+1])
	r.pos += 2
	return v, nil
}

func (r *reader) u32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := uint32(r.b[r.pos])<<24 | uint32(r.b[r.pos+1])<<16 | uint32(r.b[r.pos+2])<<8 | uint32(r.b[r.pos+3])
	r.pos += 4
	return v, nil
}

// resBitU15 对照 rwfGetResBitU15：首字节最高位=1 → 2 字节大端 & 0x7FFF。
func (r *reader) resBitU15() (uint16, error) {
	b, err := r.u8()
	if err != nil {
		return 0, err
	}
	if b&0x80 == 0 {
		return uint16(b), nil
	}
	r.pos-- // 退回按 2 字节大端读
	v, err := r.u16()
	if err != nil {
		return 0, err
	}
	return v & 0x7FFF, nil
}

// resBitU30 对照 RWF_GET_RESBIT_U30（rwfNet.h:3963-3987）：
// 首字节高 2 位 00/80/40/C0 分别对应 1/2/3/4 字节，余 6 位为最高位。
func (r *reader) resBitU30() (uint32, error) {
	b0, err := r.u8()
	if err != nil {
		return 0, err
	}
	switch b0 & 0xC0 {
	case 0x00:
		return uint32(b0), nil
	case 0x80:
		b1, err := r.u8()
		if err != nil {
			return 0, err
		}
		return uint32(b0&0x3F)<<8 | uint32(b1), nil
	case 0x40:
		b1, err := r.u8()
		if err != nil {
			return 0, err
		}
		b2, err := r.u8()
		if err != nil {
			return 0, err
		}
		return uint32(b0&0x3F)<<16 | uint32(b1)<<8 | uint32(b2), nil
	default: // 0xC0
		b1, err := r.u8()
		if err != nil {
			return 0, err
		}
		b2, err := r.u8()
		if err != nil {
			return 0, err
		}
		b3, err := r.u8()
		if err != nil {
			return 0, err
		}
		return uint32(b0&0x3F)<<24 | uint32(b1)<<16 | uint32(b2)<<8 | uint32(b3), nil
	}
}

// buffer15 对照 _rsslDecodeBuffer15：resBitU15 长度 + 内容。
func (r *reader) buffer15() ([]byte, error) {
	n, err := r.resBitU15()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	if err := r.need(int(n)); err != nil {
		return nil, err
	}
	v := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return v, nil
}

// buffer16 对照 rwfGetBuffer16：<0xFE → 1 字节长度；否则 0xFE + 2 字节大端长度。
func (r *reader) buffer16() ([]byte, error) {
	b, err := r.u8()
	if err != nil {
		return nil, err
	}
	var n int
	if b < 0xFE {
		n = int(b)
	} else {
		v, err := r.u16()
		if err != nil {
			return nil, err
		}
		n = int(v)
	}
	if n == 0 {
		return nil, nil
	}
	if err := r.need(n); err != nil {
		return nil, err
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}
