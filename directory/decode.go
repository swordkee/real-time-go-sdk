package directory

import (
	"fmt"

	"github.com/swordkee/real-time-go-sdk/rwf"
)

// 下列常量逐条取自 LSEG RTSDK C 头文件（不做「差不多就行」的取值）：
//   - Codec/rtr/rsslDataTypeEnums.h:36,53,54,85,87,92,94,96
//   - Codec/rtr/rsslMap.h:33-37（flags）、150-152（action）
//   - Codec/rtr/rsslFilterList.h:34-35（list flags）、111-112（entry flags）、139-141（action）
//   - Codec/rtr/rsslElementList.h:35-38（ELF_*）
//   - Codec/rtr/rsslRDM.h:493（RDM_DIRECTORY_SERVICE_INFO_ID=1）
const (
	dtUInt        uint8 = 4   // RSSL_DT_UINT
	dtBuffer      uint8 = 16  // RSSL_DT_BUFFER
	dtASCIIString uint8 = 17  // RSSL_DT_ASCII_STRING
	dtNoData      uint8 = 128 // RSSL_DT_NO_DATA（element dataType 不缩放，见 elemListEncoder.c:368）

	// 容器类型线上值 = 枚举值 - RSSL_DT_CONTAINER_TYPE_MIN(128)（mapDecoder.c:44、filterListDecoder.c:42）
	ctElementListWire uint8 = 5 // RSSL_DT_ELEMENT_LIST(133) - 128
	ctFilterListWire  uint8 = 7 // RSSL_DT_FILTER_LIST(135) - 128
	ctNoDataWire      uint8 = 0 // RSSL_DT_NO_DATA(128) - 128

	mpfHasSetDefs      uint8 = 0x01 // RSSL_MPF_HAS_SET_DEFS
	mpfHasSummaryData  uint8 = 0x02 // RSSL_MPF_HAS_SUMMARY_DATA
	mpfHasPerEntryPerm uint8 = 0x04 // RSSL_MPF_HAS_PER_ENTRY_PERM_DATA
	mpfHasTotalCount   uint8 = 0x08 // RSSL_MPF_HAS_TOTAL_COUNT_HINT
	mpfHasKeyFieldID   uint8 = 0x10 // RSSL_MPF_HAS_KEY_FIELD_ID

	mapEntryHasPermData uint8 = 0x01 // 高 4 位里的 bit0（mapDecoder.c:148 flags>>4 后判断）

	mapActionUpdate uint8 = 1 // RSSL_MPEA_UPDATE_ENTRY
	mapActionAdd    uint8 = 2 // RSSL_MPEA_ADD_ENTRY
	mapActionDelete uint8 = 3 // RSSL_MPEA_DELETE_ENTRY

	ftfHasPerEntryPerm uint8 = 0x01 // RSSL_FTF_HAS_PER_ENTRY_PERM_DATA
	ftfHasTotalCount   uint8 = 0x02 // RSSL_FTF_HAS_TOTAL_COUNT_HINT
	ftefHasPermData    uint8 = 0x01 // RSSL_FTEF_HAS_PERM_DATA
	ftefHasContainer   uint8 = 0x02 // RSSL_FTEF_HAS_CONTAINER_TYPE

	filterActionClear uint8 = 3 // RSSL_FTEA_CLEAR_ENTRY

	elfHasElementListInfo uint8 = 0x01 // RSSL_ELF_HAS_ELEMENT_LIST_INFO
	elfHasSetData         uint8 = 0x02 // RSSL_ELF_HAS_SET_DATA
	elfHasStandardData    uint8 = 0x08 // RSSL_ELF_HAS_STANDARD_DATA

	serviceInfoFilterID uint8 = 1 // RDM_DIRECTORY_SERVICE_INFO_ID

	elementNameName = "Name" // rsslRDM.h:950 RSSL_ENAME_NAME = {4,"Name"}
)

// decodeServiceMap 解 directory 消息体的 MAP。约束逐条对齐 C：
//   - Impl/RDM/rsslRDMDirectoryMsg.c:852/867：keyPrimitiveType 必须 UINT，containerType
//     必须 FILTER_LIST 或 NO_DATA，否则 RSSL_RET_FAILURE；
//   - Impl/Codec/mapDecoder.c:39-106：header 各可选字段顺序；
//   - Impl/Codec/mapDecoder.c:112-200：entry = action/flags + [permData] + key(buffer15) + [value(buffer16)]。
func decodeServiceMap(body []byte) ([]Service, error) {
	r := &reader{b: body}
	flags, err := r.u8()
	if err != nil {
		return nil, err
	}
	keyType, err := r.u8()
	if err != nil {
		return nil, err
	}
	ct, err := r.u8()
	if err != nil {
		return nil, err
	}
	if keyType != dtUInt {
		return nil, fmt.Errorf("%w: map key 类型 %d 非 UINT(4)（rsslRDMDirectoryMsg.c:852）", rwf.ErrInvalidData, keyType)
	}
	if ct != ctFilterListWire && ct != ctNoDataWire {
		return nil, fmt.Errorf("%w: map container %d 非 FILTER_LIST(7)/NO_DATA(0)（rsslRDMDirectoryMsg.c:852）", rwf.ErrInvalidData, ct)
	}

	// mapDecoder.c:49-95 的可选字段，顺序不可调换。
	if flags&mpfHasKeyFieldID != 0 {
		if _, err := r.u16(); err != nil {
			return nil, err
		}
	}
	if flags&mpfHasSetDefs != 0 {
		if _, err := r.buffer15(); err != nil {
			return nil, err
		}
	}
	if flags&mpfHasSummaryData != 0 {
		if _, err := r.buffer15(); err != nil {
			return nil, err
		}
	}
	if flags&mpfHasTotalCount != 0 {
		if _, err := r.resBitU30(); err != nil {
			return nil, err
		}
	}
	count, err := r.u16() // mapDecoder.c:97 itemCount
	if err != nil {
		return nil, err
	}

	services := make([]Service, 0, count)
	for i := 0; i < int(count); i++ {
		af, err := r.u8()
		if err != nil {
			return nil, err
		}
		svc, err := decodeMapEntry(r, af&0x0F, af>>4, flags, ct) // mapDecoder.c:147-148
		if err != nil {
			return nil, fmt.Errorf("第 %d 条 service: %w", i, err)
		}
		services = append(services, svc)
	}
	return services, nil
}

// decodeMapEntry 解单条 MAP entry。serviceId 从 buffer15 包装的 UInt key 取
// （DirectoryServiceStore.cpp:2030/2033 的 RsslUInt64 serviceId）。
func decodeMapEntry(r *reader, action, entryFlags, mapFlags, mapCT uint8) (Service, error) {
	// mapDecoder.c:150-161 可选 per-entry permData
	if mapFlags&mpfHasPerEntryPerm != 0 && entryFlags&mapEntryHasPermData != 0 {
		if _, err := r.buffer15(); err != nil {
			return Service{}, err
		}
	}

	key, err := r.buffer15() // mapDecoder.c:163
	if err != nil {
		return Service{}, err
	}
	serviceID, err := rwf.DecodeUInt(key) // mapDecoder.c:170 rsslDecodeUInt(RSSL_DT_UINT)
	if err != nil {
		return Service{}, fmt.Errorf("serviceId key: %w", err)
	}

	var a Action
	switch action {
	case mapActionUpdate:
		a = ActionUpdate
	case mapActionAdd:
		a = ActionAdd
	case mapActionDelete:
		a = ActionDelete
	default:
		// 未知 action fail-closed，绝不静默当成 ADD/UPDATE。
		return Service{}, fmt.Errorf("%w: 未知 map action %d（serviceId=%d）", rwf.ErrInvalidData, action, serviceID)
	}

	// mapDecoder.c:178-187：DELETE 或 NO_DATA 无 value。DELETE 仍要交付给上层清映射。
	if action == mapActionDelete {
		return Service{ServiceID: serviceID, Action: a}, nil
	}
	if mapCT == ctNoDataWire {
		return Service{}, fmt.Errorf("%w: serviceId %d 的 map 容器为 NO_DATA，无 SERVICE_INFO 可取 name", rwf.ErrInvalidData, serviceID)
	}

	value, err := r.buffer16() // mapDecoder.c:191
	if err != nil {
		return Service{}, err
	}
	name, err := decodeServiceName(value)
	if err != nil {
		return Service{}, fmt.Errorf("serviceId %d: %w", serviceID, err)
	}
	if name == "" {
		// 「有 serviceId 没 name」绝不能静默成功（rsslRDMDirectoryMsg.c:1124 的
		// foundServiceInfoName 检查对应 RSSL_RET_FAILURE）。
		return Service{}, fmt.Errorf("%w: serviceId %d 有 FILTER_LIST 但缺 SERVICE_INFO/Name", rwf.ErrInvalidData, serviceID)
	}
	return Service{ServiceID: serviceID, ServiceName: name, Action: a}, nil
}

// decodeServiceName 从 MAP entry 的 FILTER_LIST payload 里取 SERVICE_INFO 里的 Name。
// 对照 Impl/Codec/filterListDecoder.c:13-146 与 Impl/RDM/rsslRDMDirectoryMsg.c:905-1126。
func decodeServiceName(payload []byte) (string, error) {
	r := &reader{b: payload}
	flags, err := r.u8()
	if err != nil {
		return "", err
	}
	ct, err := r.u8() // filterListDecoder.c:40（容器类型线上值）
	if err != nil {
		return "", err
	}
	if flags&ftfHasTotalCount != 0 { // filterListDecoder.c:44-47
		if _, err := r.u8(); err != nil {
			return "", err
		}
	}
	count, err := r.u8() // filterListDecoder.c:51
	if err != nil {
		return "", err
	}

	name := ""
	for i := 0; i < int(count); i++ {
		af, err := r.u8()
		if err != nil {
			return "", err
		}
		action, fFlags := af&0x0F, af>>4 // filterListDecoder.c:97-98
		id, err := r.u8()                // filterListDecoder.c:101
		if err != nil {
			return "", err
		}
		entryCT := ct
		if fFlags&ftefHasContainer != 0 { // filterListDecoder.c:103-112
			entryCT, err = r.u8()
			if err != nil {
				return "", err
			}
		}
		if flags&ftfHasPerEntryPerm != 0 && fFlags&ftefHasPermData != 0 { // filterListDecoder.c:114-124
			if _, err := r.buffer15(); err != nil {
				return "", err
			}
		}
		// filterListDecoder.c:126：NO_DATA 或 CLEAR 无 payload。
		if entryCT == ctNoDataWire || action == filterActionClear {
			continue
		}
		data, err := r.buffer16() // filterListDecoder.c:128
		if err != nil {
			return "", err
		}
		if id != serviceInfoFilterID {
			continue // STATE/LOAD/GROUP 等 filter 跳过，但必须完整消费其 payload。
		}
		// rsslRDMDirectoryMsg.c:927：SERVICE_INFO 必须是 ELEMENT_LIST。
		if entryCT != ctElementListWire {
			return "", fmt.Errorf("%w: SERVICE_INFO 容器 %d 非 ELEMENT_LIST(5)", rwf.ErrInvalidData, entryCT)
		}
		// 注：EMA DirectoryServiceStore.cpp:2165-2171 拒绝 UPDATE 的 INFO filter；
		// 但 C RDM decoder（rsslRDMDirectoryMsg.c:922）允许。这里按 C RDM 口径继续取 name。
		n, err := decodeNameFromElementList(data)
		if err != nil {
			return "", err
		}
		if n != "" {
			name = n
		}
	}
	return name, nil
}

// decodeNameFromElementList 解 ELEMENT_LIST 里名为 "Name" 的元素。
// 对照 Impl/Codec/elemListDecoder.c:24-162（header）与 165-252（entry），
// 以及 Impl/RDM/rsslRDMDirectoryMsg.c:929-946/1124（Name 必需、类型 ASCII_STRING|BUFFER）。
func decodeNameFromElementList(payload []byte) (string, error) {
	r := &reader{b: payload}
	flags, err := r.u8()
	if err != nil {
		return "", err
	}
	if flags&elfHasElementListInfo != 0 { // elemListDecoder.c:52-68
		infoLen, err := r.u8()
		if err != nil {
			return "", err
		}
		if err := r.need(int(infoLen)); err != nil {
			return "", err
		}
		r.pos += int(infoLen)
	}
	if flags&elfHasSetData != 0 {
		// 本地 set-def 解码链路（elemListDecoder.c:71-136）不在本库范围；宁可拒收，不猜。
		return "", fmt.Errorf("%w: ELEMENT_LIST 带 SET_DATA，本库不支持 set 定义解码", rwf.ErrInvalidData)
	}
	if flags&elfHasStandardData == 0 { // elemListDecoder.c:137-149
		return "", fmt.Errorf("%w: ELEMENT_LIST 无 STANDARD_DATA", rwf.ErrInvalidData)
	}
	count, err := r.u16() // elemListDecoder.c:143
	if err != nil {
		return "", err
	}

	name := ""
	for i := 0; i < int(count); i++ {
		nm, err := r.buffer15() // elemListDecoder.c:222
		if err != nil {
			return "", err
		}
		dt, err := r.u8() // elemListDecoder.c:226（全枚举值，不缩放）
		if err != nil {
			return "", err
		}
		var value []byte
		if dt != dtNoData { // elemListDecoder.c:228-243
			value, err = r.buffer16() // elemListDecoder.c:230
			if err != nil {
				return "", err
			}
		}
		if string(nm) != elementNameName {
			continue
		}
		// rsslRDMDirectoryMsg.c:942：Name 只接受 ASCII_STRING 或 BUFFER。
		if dt != dtASCIIString && dt != dtBuffer {
			return "", fmt.Errorf("%w: Name 元素类型 %d 非 ASCII_STRING(17)/BUFFER(16)", rwf.ErrInvalidData, dt)
		}
		if len(value) == 0 {
			return "", fmt.Errorf("%w: Name 元素为空（BLANK）", rwf.ErrInvalidData)
		}
		name = string(value)
	}
	return name, nil
}
