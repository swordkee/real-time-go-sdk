// Package directory 是 RDM Source Directory（服务目录）的纯 Go 自动发现库：
// 编码 directory REQUEST，并把 ADS 返回的 directory REFRESH/UPDATE 解成
// serviceId → serviceName 映射所需的增量（ADD/UPDATE/DELETE）。
//
// 逐条对照 LSEG RTSDK C，不为图省事造简化语义：
//   - Cpp-C/Eta/Include/RDM/rtr/rsslRDMDirectoryMsg.h:626-633 rsslInitDefaultRDMDirectoryRequest
//   - Cpp-C/Eta/Include/RDM/rtr/rsslRDMDirectoryMsg.h:497-502/520-524 请求 flags / key
//   - Cpp-C/Eta/Include/Codec/rtr/rsslRDM.h:458-466,493-494 filter 掩码与 filter ID
//   - Cpp-C/Eta/Impl/RDM/rsslRDMDirectoryMsg.c:1682-1711 REQUEST 解码（反向校对布局）
//   - Cpp-C/Ema/Src/Access/Impl/DirectoryCallbackClient.cpp:1268-1281 stream2 + filter + streaming
//   - Cpp-C/Ema/Src/Access/Impl/DirectoryServiceStore.cpp:1986-2249 MAP(UInt key)/FILTER_LIST/Name
//   - Cpp-C/Eta/Impl/Codec/{mapDecoder.c,filterListDecoder.c,elemListDecoder.c} 线上布局
//
// 不修改 rwf / omm / ripc / native：只 import rwf 的消息解码与错误哨兵。
package directory

import (
	"fmt"

	"github.com/swordkee/real-time-go-sdk/rwf"
)

// 消息类与域（Cpp-C/Eta/Include/Codec/rtr/rsslRDM.h:31-60，rsslMsg.h RsslMsgClasses）。
const (
	msgClassRequest uint8 = 1 // RSSL_MC_REQUEST

	// DomainSource 是 RDM Source Directory 域。
	//
	// ⚠️ 与工单字面值 domain=5 的冲突（已上报父会话裁决）：本地 C 权威头文件
	// Codec/rtr/rsslRDM.h:33 定义 RSSL_DMT_SOURCE=4（目录），5 是 RSSL_DMT_DICTIONARY；
	// EmaConfigImpl.cpp:1151 也按 RSSL_DMT_SOURCE 分发目录请求。按「逐条对照 C /
	// 以原版代码为准」取 4。若父会话确认对端自定义为 5，改这一处常量即可。
	DomainSource uint8 = 4
)

// 请求 filter 掩码（rsslRDM.h:459-461）。至少 INFO|STATE；与
// rsslRDMDirectoryMsg.h:631 的 rsslInitDefaultRDMDirectoryRequest 默认值一致。
const (
	FilterServiceInfo  uint32 = 0x00000001 // RDM_DIRECTORY_SERVICE_INFO_FILTER
	FilterServiceState uint32 = 0x00000002 // RDM_DIRECTORY_SERVICE_STATE_FILTER
	FilterServiceGroup uint32 = 0x00000004 // RDM_DIRECTORY_SERVICE_GROUP_FILTER
	FilterLoad         uint32 = 0x00000008 // RDM_DIRECTORY_SERVICE_LOAD_FILTER
	FilterData         uint32 = 0x00000010 // RDM_DIRECTORY_SERVICE_DATA_FILTER
	FilterLink         uint32 = 0x00000020 // RDM_DIRECTORY_SERVICE_LINK_FILTER
)

// DefaultFilter 是 rsslInitDefaultRDMDirectoryRequest 的 typical filter（INFO|STATE|GROUP）。
const DefaultFilter uint32 = FilterServiceInfo | FilterServiceState | FilterServiceGroup

// DefaultStreamID 是 EMA 的固定 directory 流（DirectoryCallbackClient.cpp:1270）。
const DefaultStreamID int32 = 2

// key flags / request flags（rsslMsgKey.h、rsslRequestMsg.h，实测 3.10.1）。
const (
	reqKeyHasFilter  uint16 = 0x0008 // RSSL_MKF_HAS_FILTER
	requestStreaming uint16 = 0x0004 // RSSL_RQMF_STREAMING
)

// Action 是 MAP entry 的动作，取值直接采用 C 的 RsslMapEntryActions
// （rsslMap.h:150-152），便于与 C 侧日志/枚举一一对账。
type Action uint8

const (
	ActionUnknown Action = 0
	ActionUpdate  Action = 1 // RSSL_MPEA_UPDATE_ENTRY
	ActionAdd     Action = 2 // RSSL_MPEA_ADD_ENTRY
	ActionDelete  Action = 3 // RSSL_MPEA_DELETE_ENTRY
)

// Service 是一条 directory 增量。DELETE 不带 ServiceName（C 的 DELETE 无 payload），
// 由上层按 ServiceID 清除已有映射；ADD/UPDATE 必带 ServiceName。
type Service struct {
	ServiceID   uint64 // MAP key（UInt64 serviceId，DirectoryServiceStore.cpp:2030）
	ServiceName string // SERVICE_INFO filter 的 "Name" 元素
	Action      Action
}

// Request 编码一条 RDM directory REQUEST（无 key.name，只带 HAS_FILTER，
// 与 rsslInitDefaultRDMDirectoryRequest 一致）。典型调用 Request(DefaultStreamID)。
//
// 线上布局（与 rwf/encode 的 request 编码器同源，rsslEncodeMsgInternal）：
//
//	uint16 headerSize
//	uint8  msgClass=1, domain=DomainSource
//	int32  streamId
//	u15    flags=RSSL_RQMF_STREAMING(0x0004)
//	uint8  containerType=NO_DATA(0)
//	uint16 0x8000|keySize（恒 2 字节 RB15）
//	key:   u15(HAS_FILTER=0x0008) + u32(filter)
func Request(streamID int32) ([]byte, error) {
	return RequestWithFilter(streamID, DefaultFilter)
}

// RequestWithFilter 与 Request 相同，但允许调用方指定 filter（如 EMA 默认 0x3F）。
func RequestWithFilter(streamID int32, filter uint32) ([]byte, error) {
	const keySize = 1 + 4 // u15(keyFlags) + u32(filter)

	b := make([]byte, 0, 2+1+1+4+1+1+2+keySize)
	b = append(b, 0, 0) // headerSize 稍后回填
	b = append(b, msgClassRequest, DomainSource)
	b = appendU32(b, uint32(streamID))
	b = appendU15(b, requestStreaming)
	b = append(b, ctNoDataWire)
	keyLenPos := len(b)
	b = append(b, 0, 0x80) // 恒 2 字节 RB15
	b = appendU15(b, reqKeyHasFilter)
	b = appendU32(b, filter)

	putU16At(b, keyLenPos, uint16(keySize)|0x8000)
	putU16At(b, 0, uint16(len(b)-2))
	return b, nil
}

// DecodeResponse 解一条完整 RWF directory 消息（带 2 字节 headerSize 前缀，
// 即 rwf.DecodeMsg 的输入），返回 service 增量列表。
//
// fail-closed：非 directory 域/非 REFRESH|UPDATE/非 MAP、非法长度、未知
// MAP action、缺 SERVICE_INFO/Name 一律返回 error，绝不返回「有 id 没 name」的
// Service。DELETE 会正常交付（ActionDelete、ServiceName 为空）供上层清映射。
func DecodeResponse(payload []byte) ([]Service, error) {
	msg, err := rwf.DecodeMsg(payload)
	if err != nil {
		return nil, fmt.Errorf("directory: 解码 RWF 消息失败: %w", err)
	}
	if msg.Domain != DomainSource {
		return nil, fmt.Errorf("%w: domain=%d 非 SOURCE(%d)", rwf.ErrInvalidData, msg.Domain, DomainSource)
	}
	if msg.Class != rwf.MsgClassRefresh && msg.Class != rwf.MsgClassUpdate {
		return nil, fmt.Errorf("%w: msgClass=%d 非 REFRESH/UPDATE", rwf.ErrInvalidData, msg.Class)
	}
	if msg.ContainerType != rwf.DataMap {
		return nil, fmt.Errorf("%w: containerType=%d 非 MAP(137)", rwf.ErrInvalidData, msg.ContainerType)
	}
	return decodeServiceMap(msg.Body)
}

// ---- 编码原语（大端 / 网络序，与 rwf/encode 内部原语同义，本包不修改 rwf） ----

func appendU15(b []byte, v uint16) []byte {
	if v < 0x80 {
		return append(b, byte(v))
	}
	return append(b, byte(v>>8)|0x80, byte(v))
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func putU16At(b []byte, pos int, v uint16) {
	b[pos] = byte(v >> 8)
	b[pos+1] = byte(v)
}
