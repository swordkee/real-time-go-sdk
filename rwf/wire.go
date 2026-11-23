// Package rwf 是 LSEG RWF (Reuters Wire Format) 的**纯 Go** 解码实现，不依赖
// cgo / 任何闭源库。目标是验证「纯 Go 一比一把 ETA C 的 RWF/OMM 编解码翻
// 译过来」这条路的可行性（见 docs/eta-pure-go-poc.md）。
//
// 编码规则逐条对照 RTSDK C 源码：
//   - Impl/Util/Include/rtr/rwfNetwork.h  rwfGet8/16/32/64、rwfGetResBitU15、
//     rwfGetBuffer8/16、rwfGetOptByteU16、rwfGetLenSpecU64Size/I64Size
//   - Impl/Codec/messageDecoders.c       rsslDecodeMsgHeader / rsslDecodeBaseKey / 各 msg class 分支
//   - Impl/Codec/fieldListDecoder.c      rsslDecodeFieldList / rsslDecodeFieldEntry
//   - Impl/Codec/primitiveDecoders.c     rsslDecodeReal/UInt/Int/Enum/Date/Time/DateTime/Buffer
//
// 长度前缀说明（容易搞错，单独记）：
//   - 「Size 变体」（rsslDecodeUInt/DecodeInt/DecodeEnum 用）入参是**剩余字节数**，
//     不含长度字节，直接按该字节数读大端值。
//   - 「lenSpec 变体」（rwfGetLenSpecU64 等）入参是**线上首字节**，首字节即长度，
//     后面跟 1..8 字节大端值。
//   - field entry 的值是 buffer16（1 或 3 字节长度前缀），解出 payload 后交给 Size 变体。
package rwf

import "errors"

// ErrIncompleteData 数据不足（截断报文）。对应 RSSL_RET_INCOMPLETE_DATA。
var ErrIncompleteData = errors.New("rwf: 报文被截断（INCOMPLETE_DATA）")

// ErrInvalidData 数据非法（长度/类型越界）。对应 RSSL_RET_INVALID_DATA。
var ErrInvalidData = errors.New("rwf: 报文非法（INVALID_DATA）")

// ErrEndOfContainer 字段表条目已读完。对应 RSSL_RET_END_OF_CONTAINER。
var ErrEndOfContainer = errors.New("rwf: 容器结束（END_OF_CONTAINER）")

// reader 在 byte 切片上的大端游标（RWF 全部是大端 / 网络序）。
type reader struct {
	b   []byte
	pos int
}

func (r *reader) remaining() int { return len(r.b) - r.pos }

func (r *reader) need(n int) error {
	if n < 0 || r.pos+n > len(r.b) {
		return ErrIncompleteData
	}
	return nil
}

// u8 / u16 / u32 / u64 读无符号大端整数（rwfGet8/16/32/64）。
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

// resBitU15 读 15 位保留位无符号数：首字节最高位=1 表示用 2 字节（&0x7FFF）。
// 对应 rwfGetResBitU15。
func (r *reader) resBitU15() (uint16, error) {
	b, err := r.u8()
	if err != nil {
		return 0, err
	}
	if b&0x80 == 0 {
		return uint16(b), nil
	}
	r.pos-- // 退回，按 2 字节大端读
	v, err := r.u16()
	if err != nil {
		return 0, err
	}
	return v & 0x7FFF, nil
}

// resBitI15 读 15 位保留位**有符号**数（rwfGetResBitI15）：
//
//	1 字节：bit6 置位表示负（0x40）→ sign-extend；否则无符号。
//	2 字节：bit6 置位 → 原样（bit15 已是符号位）；否则清掉 bit15。
func (r *reader) resBitI15() (int16, error) {
	b0, err := r.u8()
	if err != nil {
		return 0, err
	}
	if b0&0x80 != 0 {
		b1, err := r.u8()
		if err != nil {
			return 0, err
		}
		v := int16(uint16(b0)<<8 | uint16(b1))
		if b0&0x40 == 0 {
			v &= 0x7FFF // 清掉保留位
		}
		return v, nil
	}
	if b0&0x40 != 0 { // 负数：单字节窗口是 bit0..bit5，先符号扩展
		return int16(int8(b0 | 0x80)), nil
	}
	return int16(b0), nil
}

// optByteU16 读「<0xFE 即 1 字节，否则 0xFE + 2 字节大端」（rwfGetOptByteU16）。
func (r *reader) optByteU16() (uint16, error) {
	b, err := r.u8()
	if err != nil {
		return 0, err
	}
	if b < 0xFE {
		return uint16(b), nil
	}
	return r.u16()
}

// buffer8 读「1 字节长度 + 内容」的缓冲（rwfGetBuffer8）。长度 0 → 空。
func (r *reader) buffer8() ([]byte, error) {
	n, err := r.u8()
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

// buffer16 读 rwfGetBuffer16 形态的缓冲：首字节 <0xFE → 1 字节长度；否则 2 字节大端长度。
// （注：C 实现里首字节为 0 时按非法长度 0 处理，这里同样返回空。）
func (r *reader) buffer16() ([]byte, error) {
	b, err := r.u8()
	if err != nil {
		return nil, err
	}
	var n int
	switch {
	case b < 0xFE:
		n = int(b)
	default:
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

// buffer15 读「ResBitU15 长度 + 内容」的缓冲（_rsslDecodeBuffer15）。
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

// beUintSize 把 size 字节大端无符号整数读出来（rwfGetLenSpecU64Size 语义）。
func beUintSize(b []byte, size int) (uint64, error) {
	if size < 0 || size > 8 || size > len(b) {
		return 0, ErrIncompleteData
	}
	var v uint64
	for i := 0; i < size; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, nil
}

// beIntSize 同上，但按 size 字节符号扩展（rwfGetLenSpecI64Size 语义）。
func beIntSize(b []byte, size int) (int64, error) {
	u, err := beUintSize(b, size)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		return 0, nil
	}
	if size < 8 && b[0]&0x80 != 0 {
		u |= ^uint64(0) << (uint(size) * 8) // 符号扩展
	}
	return int64(u), nil
}
