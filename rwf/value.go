package rwf

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件对照 Impl/Codec/primitiveDecoders.c 与 Impl/Codec/rwfConvert.c（Real→字符串）。

// Real hint 常量（rsslReal.h RsslRealHints）。
const (
	RealHintMinExponent = 0
	RealHintExponent0   = 14
	RealHintMaxExponent = 21
	RealHintMinDivisor  = 22
	RealHintFraction1   = 22
	RealHintFraction256 = 30
	RealHintInfinity    = 33
	RealHintNegInfinity = 34
	RealHintNotANumber  = 35
)

// Real 解出的 REAL 值。
type Real struct {
	Blank bool
	Hint  uint8
	Value int64
}

// String 按 rwfReal64tosOpts 的规则转成十进制字符串（与 rsslRealToString 输出一致）：
//   - hint 在 [EXPONENT_14, EXPONENT7]（0..21）：value * 10^(hint-14)，小数位对齐；
//   - hint 在 [FRACTION_1, FRACTION_256]（22..30）：value/2^(hint-22)，输出带分数或整数；
//   - INF/NEG_INF/NAN 输出 Inf / -Inf / NaN；
//   - blank 输出空串。
func (r Real) String() string {
	if r.Blank {
		return ""
	}
	switch r.Hint {
	case RealHintInfinity:
		return "Inf"
	case RealHintNegInfinity:
		return "-Inf"
	case RealHintNotANumber:
		return "NaN"
	}
	neg := r.Value < 0
	var abs uint64
	if neg {
		abs = uint64(-r.Value)
	} else {
		abs = uint64(r.Value)
	}

	if r.Hint >= RealHintFraction1 {
		n := int(r.Hint - RealHintFraction1) // 分母 2^n
		den := uint64(1) << uint(n)
		whole := abs / den
		rem := abs % den
		var s string
		if rem == 0 {
			s = strconv.FormatUint(whole, 10)
		} else {
			s = strconv.FormatUint(rem, 10) + "/" + strconv.FormatUint(den, 10)
			if whole > 0 {
				s = strconv.FormatUint(whole, 10) + " " + s
			}
		}
		if neg {
			return "-" + s
		}
		return s
	}

	// 指数域：base-10
	exp := int(r.Hint) - RealHintExponent0
	digits := strconv.FormatUint(abs, 10)
	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	if exp >= 0 {
		sb.WriteString(digits)
		sb.WriteString(strings.Repeat("0", exp))
		if exp > 0 && abs == 0 {
			// value==0 时 C 版输出单个 "0"（不给尾部补零）
			return trimZeroPadded(sb.String(), neg)
		}
		return sb.String()
	}
	// exp < 0：插入小数点
	frac := -exp
	if len(digits) <= frac {
		sb.WriteString("0.")
		sb.WriteString(strings.Repeat("0", frac-len(digits)))
		sb.WriteString(digits)
	} else {
		cut := len(digits) - frac
		sb.WriteString(digits[:cut])
		sb.WriteByte('.')
		sb.WriteString(digits[cut:])
	}
	return sb.String()
}

// trimZeroPadded 处理 value==0 且 exp>0 的情况：C 里输出 "0"。
func trimZeroPadded(s string, neg bool) string {
	if neg {
		return "-0"
	}
	return "0"
}

// DecodeReal 对照 rsslDecodeReal（payload 长度决定编码形态）：
//
//	0 字节 → blank；
//	1 字节 → 只有 format（可能 INF/NAN，否则 blank）；
//	其它 → rwfGetReal64（首字节是 format，后面是定点小数）。
func DecodeReal(payload []byte) (Real, error) {
	var v Real
	switch len(payload) {
	case 0:
		v.Blank = true
		return v, nil
	case 1:
		f := payload[0]
		switch f & 0x3F {
		case RealHintInfinity, RealHintNegInfinity, RealHintNotANumber:
			v.Hint = f & 0x3F
			v.Blank = false
		default:
			v.Blank = true
		}
		return v, nil
	}
	value, format, ok := getReal64(payload)
	if !ok {
		return v, ErrInvalidData
	}
	v.Value = value
	switch format & 0x3F {
	case RealHintInfinity, RealHintNegInfinity, RealHintNotANumber:
		v.Hint = format & 0x3F
		v.Blank = false
	default:
		v.Blank = (format & 0x20) != 0
		v.Hint = format & 0x1F
	}
	return v, nil
}

// getReal64 对照 rwfGetReal64：首字节 format，后面 length-1 字节大端符号数。
// （rwfGetReal64(value, format, length, sptr)，length=len(payload)）
func getReal64(payload []byte) (value int64, format uint8, ok bool) {
	if len(payload) == 0 {
		return 0, 0, false
	}
	format = payload[0]
	switch len(payload) {
	case 1:
		return 0, 0x20, true
	case 2, 3, 4, 5, 6, 7, 8, 9:
		n := len(payload) - 1
		v, err := beIntSize(payload[1:], n)
		if err != nil {
			return 0, 0, false
		}
		return v, format, true
	}
	return 0, 0, false
}

// DecodeUInt 对照 rsslDecodeUInt（Size 变体：入参=剩余字节数，不含长度字节）。
func DecodeUInt(payload []byte) (uint64, error) {
	if len(payload) == 0 {
		return 0, ErrIncompleteData
	}
	return beUintSize(payload, len(payload))
}

// DecodeInt 对照 rsslDecodeInt。
func DecodeInt(payload []byte) (int64, error) {
	if len(payload) == 0 {
		return 0, ErrIncompleteData
	}
	return beIntSize(payload, len(payload))
}

// DecodeEnum 对照 rsslDecodeEnum（rwfGetLenSpecU16_Size）。
func DecodeEnum(payload []byte) (uint16, error) {
	if len(payload) == 0 {
		return 0, ErrIncompleteData
	}
	n := len(payload)
	if n > 2 {
		return 0, ErrInvalidData
	}
	v, err := beUintSize(payload, n)
	if err != nil {
		return 0, err
	}
	return uint16(v), nil
}

// Date 解出的日期。
type Date struct {
	Year  uint16
	Month uint8
	Day   uint8
}

func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

// DecodeDate 对照 rsslDecodeDate（固定 4 字节：day month year(2)）。
func DecodeDate(payload []byte) (Date, error) {
	var d Date
	switch len(payload) {
	case 0:
		return d, nil
	case 4:
		d.Day = payload[0]
		d.Month = payload[1]
		d.Year = uint16(payload[2])<<8 | uint16(payload[3])
		return d, nil
	}
	return d, ErrIncompleteData
}

// Time 解出的时间（含毫秒时只保留时分秒用于展示）。
type Time struct {
	Hour        uint8
	Minute      uint8
	Second      uint8
	Millisecond uint16
}

func (t Time) String() string { return fmt.Sprintf("%02d:%02d:%02d", t.Hour, t.Minute, t.Second) }

// DecodeTime 对照 rsslDecodeTime（2..8 字节）。
func DecodeTime(payload []byte) (Time, error) {
	var t Time
	switch len(payload) {
	case 0:
		return t, nil
	case 1, 4:
		return t, ErrIncompleteData
	case 2:
		t.Hour = payload[0]
		t.Minute = payload[1]
		return t, nil
	case 3:
		t.Hour = payload[0]
		t.Minute = payload[1]
		t.Second = payload[2]
		return t, nil
	case 5, 6, 7, 8:
		t.Hour = payload[0]
		t.Minute = payload[1]
		t.Second = payload[2]
		t.Millisecond = uint16(payload[3])<<8 | uint16(payload[4])
		return t, nil
	}
	return t, ErrIncompleteData
}

// DateTime 解出的日期时间。
type DateTime struct {
	Date Date
	Time Time
}

func (d DateTime) String() string { return d.Date.String() + " " + d.Time.String() }

// DecodeDateTime 对照 rsslDecodeDateTime（6..12 字节）。
func DecodeDateTime(payload []byte) (DateTime, error) {
	var d DateTime
	switch len(payload) {
	case 0:
		return d, nil
	case 6, 7, 8, 9, 10, 11, 12:
		d.Date.Day = payload[0]
		d.Date.Month = payload[1]
		d.Date.Year = uint16(payload[2])<<8 | uint16(payload[3])
		d.Time.Hour = payload[4]
		d.Time.Minute = payload[5]
		if len(payload) >= 7 {
			d.Time.Second = payload[6]
		}
		if len(payload) >= 9 {
			d.Time.Millisecond = uint16(payload[7])<<8 | uint16(payload[8])
		}
		return d, nil
	}
	return d, ErrIncompleteData
}

// DecodeBuffer 对照 rsslDecodeBuffer：payload 就是内容本身。
func DecodeBuffer(payload []byte) ([]byte, error) {
	return payload, nil
}
