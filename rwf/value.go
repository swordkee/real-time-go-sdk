package rwf

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件逐条对照 Impl/Codec/primitiveDecoders.c 的
// rsslDecodeReal / rsslDecodeUInt / rsslDecodeInt / rsslDecodeEnum /
// rsslDecodeDate / rsslDecodeTime / rsslDecodeDateTime / rsslDecodeBuffer，
// 以及 Impl/Util/Include/rtr/rwfNetwork.h 的 rwfGetLenSpec*Size / rwfGetReal64。
//
// 返回码与 C 一一对应：
//   - 0 长度 → ErrBlankData（RSSL_RET_BLANK_DATA）
//   - 非法长度/非法 hint → ErrIncompleteData / ErrInvalidData
//   - 值「全 255」这类 C 用 BLANK_DATA 表示的空值 → ErrBlankData（值仍写入）

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

// DecodeReal 对照 rsslDecodeReal（primitiveDecoders.c:983-1043）：
//
//	0 字节 → BLANK_DATA；
//	1 字节 → 只有 format：INF/NEG_INF/NAN 为 SUCCESS，其余为 blank（SUCCESS + isBlank）；
//	>1 字节 → rwfGetReal64，随后校验 hint ∈ [14,35] 且 != 31/32，否则 INVALID_DATA。
func DecodeReal(payload []byte) (Real, error) {
	var v Real
	switch len(payload) {
	case 0:
		v.Blank = true
		return v, ErrBlankData
	case 1:
		f := payload[0]
		switch f & 0x3F {
		case RealHintInfinity, RealHintNegInfinity, RealHintNotANumber:
			v.Hint = f & 0x3F
			v.Blank = false
		default:
			v.Blank = true
			v.Hint = 0
		}
		v.Value = 0
		return v, nil // C 在 case 1 恒返回 SUCCESS（blank 也是 SUCCESS）
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
	// C: if (!(hint >= RSSL_RH_EXPONENT_14 && hint <= RSSL_RH_NOT_A_NUMBER && hint != 31 && hint != 32))
	//        return RSSL_RET_INVALID_DATA;
	// 注意 RSSL_RH_EXPONENT_14 == 0（rsslReal.h:121-146），不是 14。
	if !(v.Hint >= RealHintMinExponent && v.Hint <= RealHintNotANumber && v.Hint != 31 && v.Hint != 32) {
		return v, ErrInvalidData
	}
	return v, nil
}

// getReal64 对照 rwfGetReal64：首字节 format，后面 length-1 字节大端符号数；
// 支持的线上长度是 1..9（rwfNetwork.h:1541）。
func getReal64(payload []byte) (value int64, format uint8, ok bool) {
	if len(payload) == 0 || len(payload) > 9 {
		return 0, 0, false
	}
	format = payload[0]
	if len(payload) == 1 {
		return 0, 0x20, true
	}
	n := len(payload) - 1
	v, err := beIntSize(payload[1:], n)
	if err != nil {
		return 0, 0, false
	}
	return v, format, true
}

// DecodeUInt 对照 rsslDecodeUInt（0 长度 = BLANK_DATA）。
func DecodeUInt(payload []byte) (uint64, error) {
	if len(payload) == 0 {
		return 0, ErrBlankData
	}
	if len(payload) > 8 {
		// rwfGetLenSpecU64Size 对 size>8 返回 -1 → INCOMPLETE_DATA
		return 0, ErrIncompleteData
	}
	return beUintSize(payload, len(payload))
}

// DecodeInt 对照 rsslDecodeInt（0 长度 = BLANK_DATA）。
func DecodeInt(payload []byte) (int64, error) {
	if len(payload) == 0 {
		return 0, ErrBlankData
	}
	if len(payload) > 8 {
		return 0, ErrIncompleteData
	}
	return beIntSize(payload, len(payload))
}

// DecodeEnum 对照 rsslDecodeEnum（rwfGetLenSpecU16_Size：0 长度 = BLANK，>2 长度 = INCOMPLETE）。
func DecodeEnum(payload []byte) (uint16, error) {
	if len(payload) == 0 {
		return 0, ErrBlankData
	}
	if len(payload) > 2 {
		return 0, ErrIncompleteData
	}
	v, err := beUintSize(payload, len(payload))
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

// Blank 是否为空日期（C: day/month/year 全 0）。
func (d Date) Blank() bool { return d.Day == 0 && d.Month == 0 && d.Year == 0 }

// DecodeDate 对照 rsslDecodeDate（primitiveDecoders.c:1045-1063）：
// 0 字节 → BLANK_DATA；4 字节 → 全 0 时 BLANK_DATA，否则 SUCCESS；其它长度 INCOMPLETE_DATA。
func DecodeDate(payload []byte) (Date, error) {
	var d Date
	switch len(payload) {
	case 0:
		return d, ErrBlankData
	case 4:
		d.Day = payload[0]
		d.Month = payload[1]
		d.Year = uint16(payload[2])<<8 | uint16(payload[3])
		if d.Blank() {
			return d, ErrBlankData
		}
		return d, nil
	}
	return d, ErrIncompleteData
}

// Time 解出的时间（C 的 RsslTime：秒以下三级）。
type Time struct {
	Hour        uint8
	Minute      uint8
	Second      uint8
	Millisecond uint16
	// Microsecond/Nanosecond 仅在 7/8 字节形态存在（C 把 micro 的低 11 位与 nano 高 3 位打包）。
	Microsecond uint16
	Nanosecond  uint16
}

func (t Time) String() string { return fmt.Sprintf("%02d:%02d:%02d", t.Hour, t.Minute, t.Second) }

// blank 判定：C 用「全 255/全 65535/2047」表示空时间。
func (t Time) isWireBlank(mask uint8) bool {
	if t.Hour != 255 || t.Minute != 255 {
		return false
	}
	if mask >= 3 && t.Second != 255 {
		return false
	}
	if mask >= 5 && t.Millisecond != 65535 {
		return false
	}
	if mask >= 7 && t.Microsecond != 2047 {
		return false
	}
	if mask >= 8 && t.Nanosecond != 2047 {
		return false
	}
	return true
}

// DecodeTime 对照 rsslDecodeTime（primitiveDecoders.c:1065-1144）：
// 只接受 0/2/3/5/7/8 字节；1/4/6/其它 → INCOMPLETE_DATA。
func DecodeTime(payload []byte) (Time, error) {
	var t Time
	switch len(payload) {
	case 0:
		return t, ErrBlankData
	case 1, 4, 6:
		return t, ErrIncompleteData
	case 2:
		t.Hour, t.Minute = payload[0], payload[1]
		if t.isWireBlank(2) {
			t.Second, t.Millisecond, t.Microsecond, t.Nanosecond = 255, 65535, 2047, 2047
			return t, ErrBlankData
		}
		return t, nil
	case 3:
		t.Hour, t.Minute, t.Second = payload[0], payload[1], payload[2]
		if t.isWireBlank(3) {
			t.Millisecond, t.Microsecond, t.Nanosecond = 65535, 2047, 2047
			return t, ErrBlankData
		}
		return t, nil
	case 5:
		t.Hour, t.Minute, t.Second = payload[0], payload[1], payload[2]
		t.Millisecond = uint16(payload[3])<<8 | uint16(payload[4])
		if t.isWireBlank(5) {
			t.Microsecond, t.Nanosecond = 2047, 2047
			return t, ErrBlankData
		}
		return t, nil
	case 7:
		t.Hour, t.Minute, t.Second = payload[0], payload[1], payload[2]
		t.Millisecond = uint16(payload[3])<<8 | uint16(payload[4])
		t.Microsecond = uint16(payload[5])<<8 | uint16(payload[6])
		if t.isWireBlank(7) {
			t.Nanosecond = 2047
			return t, ErrBlankData
		}
		return t, nil
	case 8:
		t.Hour, t.Minute, t.Second = payload[0], payload[1], payload[2]
		t.Millisecond = uint16(payload[3])<<8 | uint16(payload[4])
		tempMicro := uint16(payload[5])<<8 | uint16(payload[6])
		tempNano := payload[7]
		// C: microsecond = tempMicro & 0x07FF; nanosecond = ((tempMicro & 0x3800) >> 3) + tempNano
		t.Microsecond = tempMicro & 0x07FF
		t.Nanosecond = ((tempMicro & 0x3800) >> 3) + uint16(tempNano)
		if t.isWireBlank(8) {
			return t, ErrBlankData
		}
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

// DecodeDateTime 对照 rsslDecodeDateTime（primitiveDecoders.c:1147-1255）：
// 只接受 0/6/7/9/11/12 字节；其它长度 INCOMPLETE_DATA。
func DecodeDateTime(payload []byte) (DateTime, error) {
	var d DateTime
	switch len(payload) {
	case 0:
		return d, ErrBlankData
	case 6, 7, 9, 11, 12:
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
		if len(payload) == 11 {
			d.Time.Microsecond = uint16(payload[9])<<8 | uint16(payload[10])
		}
		if len(payload) == 12 {
			tempMicro := uint16(payload[9])<<8 | uint16(payload[10])
			tempNano := payload[11]
			d.Time.Microsecond = tempMicro & 0x07FF
			d.Time.Nanosecond = ((tempMicro & 0x3800) >> 3) + uint16(tempNano)
		}
		// 逐字对照 C：先按「时间部分是否全 255」补齐秒以下字段，再结合日期全 0 判 blank。
		var timeBlank bool
		switch len(payload) {
		case 6:
			timeBlank = d.Time.Hour == 255 && d.Time.Minute == 255
			if timeBlank {
				d.Time.Second, d.Time.Millisecond = 255, 65535
				d.Time.Microsecond, d.Time.Nanosecond = 2047, 2047
			} else {
				d.Time.Second, d.Time.Millisecond = 0, 0
				d.Time.Microsecond, d.Time.Nanosecond = 0, 0
			}
		case 7:
			timeBlank = d.Time.Hour == 255 && d.Time.Minute == 255 && d.Time.Second == 255
			if timeBlank {
				d.Time.Millisecond = 65535
				d.Time.Microsecond, d.Time.Nanosecond = 2047, 2047
			} else {
				d.Time.Millisecond = 0
				d.Time.Microsecond, d.Time.Nanosecond = 0, 0
			}
		case 9:
			timeBlank = d.Time.Hour == 255 && d.Time.Minute == 255 && d.Time.Second == 255 && d.Time.Millisecond == 65535
			if timeBlank {
				d.Time.Microsecond, d.Time.Nanosecond = 2047, 2047
			} else {
				d.Time.Microsecond, d.Time.Nanosecond = 0, 0
			}
		case 11:
			timeBlank = d.Time.Hour == 255 && d.Time.Minute == 255 && d.Time.Second == 255 &&
				d.Time.Millisecond == 65535 && d.Time.Microsecond == 2047
			if timeBlank {
				d.Time.Nanosecond = 2047
			} else {
				d.Time.Nanosecond = 0
			}
		case 12:
			timeBlank = d.Time.Hour == 255 && d.Time.Minute == 255 && d.Time.Second == 255 &&
				d.Time.Millisecond == 65535 && d.Time.Microsecond == 2047 && d.Time.Nanosecond == 2047
		}
		if timeBlank && d.Date.Blank() {
			return d, ErrBlankData
		}
		return d, nil
	}
	return d, ErrIncompleteData
}

// DecodeBuffer 对照 rsslDecodeBuffer：payload 就是内容本身；0 长度 = BLANK_DATA。
func DecodeBuffer(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, ErrBlankData
	}
	return payload, nil
}
