// Package rmtes 实现 LSEG RMTES（ISO 2022 / Marketfeed）字节流到 UTF-8 的转换。
//
// 算法与转换表机械转写自 LSEG Real-Time-SDK（Apache-2.0，commit 7d219a600）：
//
//	Cpp-C/Eta/Impl/Codec/rsslRmtes.c
//	Cpp-C/Eta/Impl/Codec/rsslcnvtab.c
//	Cpp-C/Eta/Impl/Codec/rsslCharSet.c / rtr/rsslCharSet.h
//	Cpp-C/Eta/Include/Codec/rtr/rsslRmtes.h
//
// C 侧完整链路是 rsslRMTESApplyToCache（展开光标移动/重复命令）后
// rsslRMTESToUTF8（解析 CL/GL/GR、转义与多字节表）。本包同样分两阶段：
// ApplyToCache 对应前者，toUTF8FromCache 对应后者；ToUTF8 串行执行二者。
//
// 与 C 的行为差异仅在输出缓冲：C 要求调用方预分配并可能因 BUFFER_TOO_SMALL
// 失败，本 API 返回新分配的切片，故不存在该错误；其余等价（含非法序列 fail-closed）。
// 空输入 C 返回 RSSL_RET_INVALID_DATA；本 API 视作合法的空输出。
package rmtes

import (
	"errors"
	"math"
)

const (
	escChar   = 0x1B
	lbrktChar = 0x5B
	rhpaChar  = 0x60 // 光标移动命令结束符 `
	rrepChar  = 0x62 // 重复命令结束符 b
)

var (
	// ErrInvalidArgument 用于 ApplyToCache 的空输入（对应 RSSL_RET_INVALID_ARGUMENT）。
	ErrInvalidArgument = errors.New("rmtes: invalid argument")
	// ErrInvalidSequence 对应 RSSL_RET_FAILURE：非法/截断的 RMTES 序列，fail-closed。
	ErrInvalidSequence = errors.New("rmtes: invalid RMTES sequence")
)

// RMTESParseState 对应 C 的 RMTESParseState 枚举。
const (
	stateError          = -1
	stateNormal         = 0
	stateESC            = 1
	stateLBRKT          = 3
	stateRHPA           = 4
	stateRREP           = 5
	stateESC21          = 6
	stateESC22          = 7
	stateESC24          = 8
	stateESC2428        = 9
	stateESC2429        = 10
	stateESC242A        = 11
	stateESC242B        = 12
	stateESC25          = 13
	stateESC26          = 14
	stateESC2640        = 15
	stateESC2640ESC     = 16
	stateESC2640ESC24   = 17
	stateESC2640ESC2429 = 18
	stateESC2640ESC242A = 19
	stateESC2640ESC242B = 20
	stateESC28          = 21
	stateESC29          = 22
	stateESC2A          = 23
	stateESC2B          = 24
)

// ESCReturnCode 对应 C 的 ESCReturnCode 枚举。
const (
	escError   = -1
	escSuccess = 0
	utfEnc     = 1
	rhpaCmd    = 2
	rrepCmd    = 3
	endChar    = 4
)

const (
	typeRMTES = 1
	typeUTF8  = 2
)

// ToUTF8 将 RMTES 字节流转换为 UTF-8。
// 先经 ApplyToCache 展开光标移动/重复命令，再逐字符按工作表与转换表转换，
// 处理 ESC 转义、SS2/SS3 单移、表内 stride-2 多字节序列。
// 任何非法/截断序列返回 ErrInvalidSequence，不产出"尽力而为"的乱码。
func ToUTF8(input []byte) ([]byte, error) {
	if len(input) == 0 {
		return []byte{}, nil
	}
	cache, err := ApplyToCache(input)
	if err != nil {
		return nil, err
	}
	return toUTF8FromCache(cache)
}

// controlParse 对应 C 的 controlParse：解析从 data[pos] 起始的控制组序列。
// 返回消耗的字节数与 ESCReturnCode。越界一律返回 (0, escError)（fail-closed），
// 避免 C 实现中读取 endPtr 之后字节的未定义行为。
func controlParse(data []byte, pos int, ws *workingSet) (int, int) {
	n := len(data)
	if pos >= n {
		return 0, escError
	}
	state, newState := stateNormal, stateNormal
	length := 0
	i := pos

	switch data[i] {
	case 0x00: // NULL 忽略
		return 1, escSuccess
	case 0x0F: // LS0 -> GL = G0
		ws.gl = &ws.g0
		return 1, escSuccess
	case 0x0E: // LS1 -> GL = G1
		ws.gl = &ws.g1
		return 1, escSuccess
	case escChar:
		state = stateESC
		length = 1
	case 0x1C:
		return 0, endChar
	default:
		return 0, escSuccess
	}

	for {
		i++
		length++
		if i >= n {
			return 0, escError
		}
		b := data[i]
		newState = stateNormal
		switch state {
		case stateESC:
			switch b {
			case 0x21:
				newState = stateESC21
			case 0x22:
				newState = stateESC22
			case 0x24:
				newState = stateESC24
			case 0x25:
				newState = stateESC25
			case 0x26:
				newState = stateESC26
			case 0x28:
				newState = stateESC28
			case 0x29:
				newState = stateESC29
			case 0x2A:
				newState = stateESC2A
			case 0x2B:
				newState = stateESC2B
			case lbrktChar:
				newState = stateLBRKT
			case 0x6E:
				ws.gl = &ws.g2
			case 0x6F:
				ws.gl = &ws.g3
			case 0x7E:
				ws.gr = &ws.g1
			case 0x7D:
				ws.gr = &ws.g2
			case 0x7C:
				ws.gr = &ws.g3
			default:
				return 0, escError
			}
		case stateESC21:
			if b != 0x40 {
				return 0, escError
			}
		case stateESC22:
			if b != 0x30 {
				return 0, escError
			}
		case stateESC24:
			switch b {
			case 0x28:
				newState = stateESC2428
			case 0x29:
				newState = stateESC2429
			case 0x2A:
				newState = stateESC242A
			case 0x2B:
				newState = stateESC242B
			default:
				return 0, escError
			}
		case stateESC2428:
			switch b {
			case 0x47:
				ws.g0 = &charsetChinese1
			case 0x48:
				ws.g0 = &charsetChinese2
			default:
				return 0, escError
			}
		case stateESC2429:
			switch b {
			case 0x47:
				ws.g1 = &charsetChinese1
			case 0x48:
				ws.g1 = &charsetChinese2
			default:
				return 0, escError
			}
		case stateESC242A:
			switch b {
			case 0x47, 0x35:
				ws.g2 = &charsetChinese1
			case 0x48:
				ws.g2 = &charsetChinese2
			default:
				return 0, escError
			}
		case stateESC242B:
			switch b {
			case 0x47:
				ws.g3 = &charsetChinese1
			case 0x48, 0x36:
				ws.g3 = &charsetChinese2
			case 0x34:
				ws.g3 = &charsetJapaneseKanji
			default:
				return 0, escError
			}
		case stateESC25:
			if b == 0x30 {
				return length, utfEnc
			}
			return 0, escError
		case stateESC26:
			if b == 0x40 {
				newState = stateESC2640
			} else {
				return 0, escError
			}
		case stateESC2640:
			if b == escChar {
				newState = stateESC2640ESC
			} else {
				return 0, escError
			}
		case stateESC2640ESC:
			if b == 0x24 {
				newState = stateESC2640ESC24
			} else {
				return 0, escError
			}
		case stateESC2640ESC24:
			switch b {
			case 0x42:
				ws.g0 = &charsetJapaneseKanji
			case 0x29:
				newState = stateESC2640ESC2429
			case 0x2A:
				newState = stateESC2640ESC242A
			case 0x2B:
				newState = stateESC2640ESC242B
			default:
				return 0, escError
			}
		case stateESC2640ESC2429:
			if b == 0x42 {
				ws.g1 = &charsetJapaneseKanji
			} else {
				return 0, escError
			}
		case stateESC2640ESC242A:
			if b == 0x42 {
				ws.g2 = &charsetJapaneseKanji
			} else {
				return 0, escError
			}
		case stateESC2640ESC242B:
			if b == 0x42 {
				ws.g3 = &charsetJapaneseKanji
			} else {
				return 0, escError
			}
		case stateESC28:
			switch b {
			case 0x42:
				ws.g0 = &charsetReuterBasic1
			case 0x49:
				ws.g0 = &charsetJapaneseKatakana
			case 0x4A:
				ws.g0 = &charsetJapaneseLatin
			default:
				return 0, escError
			}
		case stateESC29:
			switch b {
			case 0x31:
				ws.g1 = &charsetReuterBasic2
			case 0x42:
				ws.g1 = &charsetReuterBasic1
			case 0x49:
				ws.g1 = &charsetJapaneseKatakana
			case 0x4A:
				ws.g1 = &charsetJapaneseLatin
			default:
				return 0, escError
			}
		case stateESC2A:
			if b == 0x32 {
				ws.g2 = &charsetJapaneseKatakana
			} else {
				return 0, escError
			}
		case stateESC2B:
			if b == 0x33 {
				ws.g3 = &charsetJapaneseLatin
			} else {
				return 0, escError
			}
		case stateLBRKT:
			for i < n && data[i] >= '0' && data[i] <= '9' {
				i++
				length++
			}
			if i >= n {
				return 0, escError
			}
			switch data[i] {
			case rhpaChar:
				return length, rhpaCmd
			case rrepChar:
				return length, rrepCmd
			default:
				return 0, escError
			}
		}
		state = newState
		if i >= n || newState == stateNormal {
			break
		}
	}
	return length, escSuccess
}

// appendUCS2 对应 C 的 UCS2ToUTF8，用 append 取代输出缓冲与 BUFFER_TOO_SMALL 判定。
func appendUCS2(out []byte, r uint16) []byte {
	switch {
	case r < 0x0080:
		return append(out, byte(r))
	case r < 0x0800:
		return append(out, 0xC0|byte(r>>6), 0x80|byte(r&0x3F))
	default:
		return append(out, 0xE0|byte(r>>12), 0x80|byte((r>>6)&0x3F), 0x80|byte(r&0x3F))
	}
}

// toUTF8FromCache 对应 C 的 rsslRMTESToUTF8（输入已应用过 ApplyToCache）。
func toUTF8FromCache(cache []byte) ([]byte, error) {
	var out []byte
	ws := newWorkingSet()
	var shiftGL *charSet
	encType := typeRMTES
	n := len(cache)

	for i := 0; i < n; {
		in := cache[i]
		if encType == typeRMTES {
			// MUTATION_ANCHOR_CL_BRANCH：控制字符分支（ESC/LS0/LS1/结束符）
			if in < 0x20 { // CL
				if shiftGL != nil {
					return nil, ErrInvalidSequence
				}
				ret, code := controlParse(cache, i, ws)
				if ret == 0 {
					switch code {
					case escError:
						return nil, ErrInvalidSequence
					case endChar:
						return out, nil
					case escSuccess:
						out = appendUCS2(out, uint16(in))
						i++
					default:
						return nil, ErrInvalidSequence
					}
				} else {
					if code == rrepCmd || code == rhpaCmd {
						for k := 0; k < ret; k++ {
							if i+k >= n {
								return nil, ErrInvalidSequence
							}
							out = append(out, cache[i+k])
						}
						i += ret
					} else {
						i += ret
						if code == utfEnc {
							encType = typeUTF8
						}
					}
				}
			} else if ws.glSet().shape == shape94 && in == 0x20 { // 94 集空格
				out = appendUCS2(out, 0x20)
				i++
			} else if ws.glSet().shape == shape94 && in == 0x7F { // 94 集 DEL -> U+FFFD
				out = appendUCS2(out, 0xFFFD)
				i++
			} else if in < 0x80 { // GL
				tmp := shiftGL
				if tmp == nil {
					tmp = ws.glSet()
				}
				if !isGLChar(in, tmp) {
					return nil, ErrInvalidSequence
				}
				if tmp.stride == 2 {
					if i+1 >= n {
						return nil, ErrInvalidSequence
					}
					if !isGLChar(cache[i+1], tmp) {
						return nil, ErrInvalidSequence
					}
					out = appendUCS2(out, convertStride2GL(in, cache[i+1], tmp))
					i += 2
				} else {
					out = appendUCS2(out, glConvertSingle1(in, tmp))
					if tmp.table2 != nil {
						if s := glConvertSingle2(in, tmp); s != 0 {
							out = appendUCS2(out, s)
						}
					}
					i++
				}
				shiftGL = nil
			} else if in < 0xA0 { // CR：SS2/SS3 单移，其余 -> U+FFFD
				if shiftGL != nil {
					return nil, ErrInvalidSequence
				}
				if in == 0x8E {
					shiftGL = ws.g2
				} else if in == 0x8F {
					shiftGL = ws.g3
				} else {
					out = appendUCS2(out, 0xFFFD)
				}
				i++
			} else { // GR
				if shiftGL != nil {
					return nil, ErrInvalidSequence
				}
				gr := ws.grSet()
				if !isGRChar(in, gr) {
					return nil, ErrInvalidSequence
				}
				if gr.stride == 2 {
					if i+1 >= n {
						return nil, ErrInvalidSequence
					}
					if isGRChar(cache[i+1], gr) {
						out = appendUCS2(out, convertStride2GR(in, cache[i+1], gr))
					} else {
						out = appendUCS2(out, 0xFFFD)
					}
					i += 2
				} else {
					out = appendUCS2(out, grConvertSingle1(in, gr))
					if gr.table2 != nil {
						if s := grConvertSingle2(in, gr); s != 0 {
							out = appendUCS2(out, s)
						}
					}
					i++
				}
			}
		} else { // TYPE_UTF8：ESC 后可继续出现控制命令，其余原样拷贝
			if in == escChar {
				ret, code := controlParse(cache, i, ws)
				if ret == 0 {
					return nil, ErrInvalidSequence
				}
				if code == rrepCmd || code == rhpaCmd {
					for k := 0; k < ret; k++ {
						if i+k >= n {
							return nil, ErrInvalidSequence
						}
						out = append(out, cache[i+k])
					}
				}
				i += ret
			} else {
				out = append(out, in)
				i++
			}
		}
	}
	return out, nil
}

// ApplyToCache 对应 C 的 rsslRMTESApplyToCache：
// 展开 ESC [ n ` （光标移动）与 ESC [ n b（重复前一字符 n 次）命令，
// 其余转义原样写入 cache，供 toUTF8FromCache 解析。
// 单次调用语义：cache 初始为空（C 侧 cacheBuf->length 初始为 0）。
func ApplyToCache(input []byte) ([]byte, error) {
	n := len(input)
	if n == 0 {
		return nil, ErrInvalidArgument
	}

	buf := make([]byte, 0, n+8)
	writePos, maxLen := 0, 0
	var numCount uint32
	var prevChar byte
	escStart, escFirst := 0, false
	state := stateNormal

	ensure := func(target int) {
		for len(buf) < target {
			buf = append(buf, 0)
		}
	}
	if input[0] == escChar {
		escFirst = true
		escStart = 0
	}

	for pos := 0; pos < n; pos++ {
		b := input[pos]
		switch state {
		case stateNormal:
			if b == escChar {
				state = stateESC
				escStart = pos
				numCount = 0
			} else {
				ensure(writePos + 1)
				buf[writePos] = b
				writePos++
				prevChar = b
			}
		case stateESC:
			if b == lbrktChar {
				state = stateLBRKT
			} else if b == 0x25 {
				pos++
				if pos >= n || input[pos] != 0x30 {
					return nil, ErrInvalidSequence
				}
				ensure(writePos + 3)
				buf[writePos], buf[writePos+1], buf[writePos+2] = escChar, 0x25, 0x30
				writePos += 3
				state = stateNormal
			} else {
				ensure(writePos + 2)
				buf[writePos], buf[writePos+1] = escChar, b
				writePos += 2
				state = stateNormal
			}
		case stateLBRKT:
			if b >= '0' && b <= '9' {
				tmp := uint64(numCount)*10 + uint64(b-'0')
				if tmp > math.MaxUint32 {
					return nil, ErrInvalidSequence
				}
				numCount = uint32(tmp)
			} else if b == rhpaChar {
				if escStart == 0 {
					escFirst = true
				}
				writePos = int(numCount)
				ensure(writePos)
				numCount = 0
				state = stateNormal
			} else if b == rrepChar {
				if escStart == 0 {
					escFirst = true
				}
				ensure(writePos + int(numCount))
				for k := 0; k < int(numCount); k++ {
					buf[writePos+k] = prevChar
				}
				writePos += int(numCount)
				state = stateNormal
			} else {
				return nil, ErrInvalidSequence
			}
		}
		if writePos > maxLen {
			maxLen = writePos
		}
	}
	if state != stateNormal {
		return nil, ErrInvalidSequence
	}
	_ = escFirst // C 在多批次增量场景下才用到；单次调用等价于返回 maxLen
	return buf[:maxLen], nil
}
