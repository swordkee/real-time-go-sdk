// Package encode 是 LSEG RWF (Reuters Wire Format) **请求消息的纯 Go 编码器**，
// 逐条对照 ETA C 的 Impl/Codec/messageEncoders.c（rsslEncodeMsg → rsslEncodeMsgInternal →
// rsslEncodeKeyInternal）与 Impl/Util/Include/rtr/rwfNetwork.h 的 rwfPut* 原语。
//
// 为什么要有它：ADS 对 login/item request 的编码**逐字节敏感**，错了只会回 Status 拒绝
// （不给原因）。因此本包的每个编码结果都有 C 侧黄金 hex 对拍门禁（见 encode_test.go，
// 黄金值由 LSEG RTSDK C 实现（rsslEncodeMsg）生成）。
//
// 线上布局（socket 传输、RWF 14.1；不含 RIPC 3 字节传输头）：
//
//	uint16 headerSize            // 从 msgClass 到 header 末尾（不含这 2 字节）
//	uint8  msgClass
//	uint8  domainType
//	int32  streamId              // 大端
//	<request>: uint15 flags, uint8 containerType(减 128), [priority], [qos], [worstQos],
//	           uint16 0x8000|keySize（**编码器恒写 2 字节 RB15**）, <key block>, [extendedHeader]
//	<close>  : uint15 flags, uint8 containerType(减 128), [extendedHeader]
//
// key block（rsslEncodeKeyInternal）：
//
//	uint15 flags
//	[optByteU16 serviceId]       // HAS_SERVICE_ID：<0xFE 单字节，否则 0xFE + uint16
//	[buffer8 name][uint8 nameType]  // HAS_NAME / HAS_NAME_TYPE
//	[uint32 filter] [uint32 identifier]
//
// 注意：HAS_NAME 但 name 为空时 C 会清掉 HAS_NAME（也连带清 HAS_NAME_TYPE）；
// 本包对「空 name」直接报错（fail-closed，不静默发出一个没有 key 的请求）。
package encode

import (
	"errors"
	"fmt"
)

// 消息类（rsslMsg.h RsslMsgClasses）。
const (
	MsgClassRequest uint8 = 1
	MsgClassClose   uint8 = 5
)

// 域类型（RsslDomainTypes 中本包用到的两个）。
const (
	DomainLogin       uint8 = 1
	DomainMarketPrice uint8 = 6
)

// 容器类型：NO_DATA（线上值 = 128 - RSSL_DT_CONTAINER_TYPE_MIN(128) = 0）。
const DataNoData uint8 = 128

// MsgKeyFlags（rsslMsgKey.h，已核对 3.10.1 头文件）。
const (
	MsgKeyHasServiceID uint16 = 0x0001
	MsgKeyHasName      uint16 = 0x0002
	MsgKeyHasNameType  uint16 = 0x0004
	MsgKeyHasFilter    uint16 = 0x0008
	MsgKeyHasIdent     uint16 = 0x0010
)

// RequestMsgFlags（rsslRequestMsg.h，3.10.1 实测值：STREAMING=0x0004）。
const (
	RequestStreaming = 0x0004
)

// nameType：RDM_LOGIN_USER_NAME / RIC 都是 1（RsslNameTypes 的 RSSL_NameType_RIC）。
const NameTypeRIC uint8 = 1

// name 上限（buffer8 长度是 1 字节，且 C 侧 name.length > RWF_MAX_8 报 INVALID_DATA）。
const maxNameLen = 255

var (
	// ErrEmptyName 请求必须带 key.name（本包不静默清 HAS_NAME）。
	ErrEmptyName = errors.New("encode: 请求 name 为空")
	// ErrNameTooLong name 超过 255 字节（buffer8 装不下）。
	ErrNameTooLong = errors.New("encode: 请求 name 超长（>255 字节）")
)

// MsgKey 是请求消息 key（只支持本仓用到的字段；attrib 不在此列）。
type MsgKey struct {
	ServiceID uint16 // HAS_SERVICE_ID
	Name      string // HAS_NAME
	NameType  uint8  // HAS_NAME_TYPE（仅在 Name 非空时有意义）
	Filter    uint32 // HAS_FILTER
	Ident     uint32 // HAS_IDENTIFIER
	// HasFilter/HasIdent 显式控制 optional 字段（0 值也是合法值）。
	HasFilter bool
	HasIdent  bool
	// HasServiceID 让调用方显式要求 HAS_SERVICE_ID（即使 serviceId==0，
	// 对应 C 里 key.flags 带 RSSL_MKF_HAS_SERVICE_ID 而 serviceId 为 0 的情形）。
	HasServiceID bool
}

// LoginRequest 编码 raw REQUEST/LOGIN（streamId + msgKey.name=username + nameType=RIC）。
//
// 与 C 侧 `encode_request(stream=1, domain=LOGIN, flags=HAS_NAME|HAS_NAME_TYPE,
// nameType=1, name="username", requestFlags=STREAMING)` 逐字节一致：
// `00150101000000010400800b0608757365726e616d6501`（23 字节，见
// docs/reference/eta-rwf-enc-oracle.c 的 LOGIN 输出）。
func LoginRequest(streamID int32, username string) ([]byte, error) {
	return Request(streamID, DomainLogin, MsgKey{Name: username, NameType: NameTypeRIC}, RequestStreaming)
}

// MarketPriceRequest 编码一条 MARKET_PRICE item request（带数字 serviceId）。
//
// 与 C 侧 serviceId=999 / name=TEST / stream=3 的编码逐字节一致：
// `00140106000000030400800a07fe03e7045445535401`（22 字节，见
// docs/reference/eta-rwf-enc-oracle.c 的 ITEM999 输出）。
func MarketPriceRequest(streamID int32, serviceID uint16, name string) ([]byte, error) {
	return Request(streamID, DomainMarketPrice,
		MsgKey{ServiceID: serviceID, Name: name, NameType: NameTypeRIC}, RequestStreaming)
}

// Request 编码任意 domain 的 raw REQUEST（key 里设置了什么就带什么 flag）。
func Request(streamID int32, domain uint8, key MsgKey, requestFlags uint16) ([]byte, error) {
	if key.Name == "" {
		return nil, ErrEmptyName
	}
	if len(key.Name) > maxNameLen {
		return nil, fmt.Errorf("%w: %d 字节", ErrNameTooLong, len(key.Name))
	}
	keyFlags := uint16(MsgKeyHasName)
	if key.HasServiceID || key.ServiceID > 0 {
		keyFlags |= MsgKeyHasServiceID
	}
	if key.NameType != 0 {
		keyFlags |= MsgKeyHasNameType
	}
	if key.HasFilter {
		keyFlags |= MsgKeyHasFilter
	}
	if key.HasIdent {
		keyFlags |= MsgKeyHasIdent
	}

	keySize := 0
	keySize += u15Len(keyFlags)
	if keyFlags&MsgKeyHasServiceID != 0 {
		keySize += optByteU16Len(key.ServiceID)
	}
	keySize += 1 + len(key.Name) // buffer8
	if keyFlags&MsgKeyHasNameType != 0 {
		keySize++
	}
	if keyFlags&MsgKeyHasFilter != 0 {
		keySize += 4
	}
	if keyFlags&MsgKeyHasIdent != 0 {
		keySize += 4
	}

	// 2(len) + 1(class) + 1(domain) + 4(stream) + u15(flags) + 1(container) +
	// 2(keySize RB15) + keySize
	total := 2 + 1 + 1 + 4 + u15Len(requestFlags) + 1 + 2 + keySize
	b := make([]byte, 0, total)

	b = append(b, 0, 0) // headerSize 稍后回填
	b = append(b, MsgClassRequest, domain)
	b = appendU32(b, uint32(streamID))
	b = appendU15(b, requestFlags)
	b = append(b, DataNoData-128) // 线上 containerType 值（NO_DATA → 0）
	keyLenPos := len(b)
	b = append(b, 0, 0x80) // 恒 2 字节 RB15（C 编码器 rwfPut16(lenPos, keySize|0x8000)）
	b = appendKey(b, keyFlags, key)

	if keySize != len(b)-keyLenPos-2 {
		// 自校验：预计算与实写不一致（编码器 bug）必须显式失败，不发出错字节。
		return nil, fmt.Errorf("encode: key 长度自校验失败 预算=%d 实写=%d", keySize, len(b)-keyLenPos-2)
	}
	putU16At(b, keyLenPos, uint16(keySize)|0x8000)
	putU16At(b, 0, uint16(len(b)-2))
	return b, nil
}

// CloseRequest 编码 CLOSE（streamId + domain），与 C 侧 `00 08 05 06 00000003 00 00` 一致。
// flags=0、containerType=NO_DATA、无 key。
func CloseRequest(streamID int32, domain uint8) []byte {
	b := make([]byte, 0, 10)
	b = append(b, 0, 0)
	b = append(b, MsgClassClose, domain)
	b = appendU32(b, uint32(streamID))
	b = append(b, 0) // flags（u15 单字节 0）
	b = append(b, DataNoData-128)
	putU16At(b, 0, uint16(len(b)-2))
	return b
}

// appendKey 按 rsslEncodeKeyInternal 的顺序写 key block。
func appendKey(b []byte, keyFlags uint16, key MsgKey) []byte {
	b = appendU15(b, keyFlags)
	if keyFlags&MsgKeyHasServiceID != 0 {
		b = appendOptByteU16(b, key.ServiceID)
	}
	b = append(b, byte(len(key.Name)))
	b = append(b, key.Name...)
	if keyFlags&MsgKeyHasNameType != 0 {
		b = append(b, key.NameType)
	}
	if keyFlags&MsgKeyHasFilter != 0 {
		b = appendU32(b, key.Filter)
	}
	if keyFlags&MsgKeyHasIdent != 0 {
		b = appendU32(b, key.Ident)
	}
	return b
}

// ---- rwfPut* 原语（大端 / 网络序） ----

// appendU15 对应 rwfPutResBitU15：<0x80 单字节，否则 0x8000|v 两字节大端。
func appendU15(b []byte, v uint16) []byte {
	if v < 0x80 {
		return append(b, byte(v))
	}
	return append(b, byte(v>>8)|0x80, byte(v))
}

// u15Len 返回 appendU15 的字节数。
func u15Len(v uint16) int {
	if v < 0x80 {
		return 1
	}
	return 2
}

// appendOptByteU16 对应 rwfPutOptByteU16：<0xFE 单字节，否则 0xFE + 两字节大端。
func appendOptByteU16(b []byte, v uint16) []byte {
	if v < 0xFE {
		return append(b, byte(v))
	}
	return append(b, 0xFE, byte(v>>8), byte(v))
}

// optByteU16Len 返回 appendOptByteU16 的字节数。
func optByteU16Len(v uint16) int {
	if v < 0xFE {
		return 1
	}
	return 3
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// putU16At 大端写入 b[pos:pos+2]（回填 headerSize / keySize 用）。
func putU16At(b []byte, pos int, v uint16) {
	b[pos] = byte(v >> 8)
	b[pos+1] = byte(v)
}
