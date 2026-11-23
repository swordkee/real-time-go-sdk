package rwf

// 本文件对照 Impl/Codec/fieldListDecoder.c 的 rsslDecodeFieldList / rsslDecodeFieldEntry，
// 以及 rsslDataTypeEnums.h 的数据类型枚举。

// 数据类型（rsslDataTypeEnums.h RsslDataType，仅列常用项）。
const (
	DataInt      = 3
	DataUInt     = 4
	DataFloat    = 5
	DataDouble   = 6
	DataReal     = 8
	DataDate     = 9
	DataTime     = 10
	DataDateTime = 11
	DataQos      = 12
	DataState    = 13
	DataEnum     = 14
	DataArray    = 15
	DataBuffer   = 16
	DataAsciiStr = 17
	DataUTF8Str  = 18
	DataRmtesStr = 19
)

// FieldListFlags（rsslFieldList.h）。
const (
	FieldListHasInfo    = 0x01
	FieldListHasSetData = 0x02
	FieldListHasSetID   = 0x04
	FieldListHasStdData = 0x08
)

// FieldEntry 一个字段条目（fid + 原始 payload；dataType 通常为 0，须查字典）。
type FieldEntry struct {
	FieldID  uint16
	DataType uint8
	raw      []byte // 去掉 buffer16 长度前缀后的 payload
}

// Raw 返回 payload 副本（调用方可自行解析，如 rmtles）。
func (f FieldEntry) Raw() []byte { return f.raw }

// FieldList 解出的字段表头 + 条目。
type FieldList struct {
	Flags        uint8
	DictionaryID int16
	FieldListNum int16
	SetID        uint16
	// Entries 按线上顺序；SetData 内容本 PoC 不展开（本 ADS 用的是标准 field list）。
	Entries []FieldEntry
}

// DecodeFieldList 对照 rsslDecodeFieldList：解 FIELD_LIST 容器体，并按条目切好 payload。
//
// 说明：C 版把「切条目」和「解值」拆成两步（decodeFieldEntry 只切 payload，值由
// 调用方用 rsslDecodeXxx 在条目 payload 上解）。这里同样两步，保持同构。
func DecodeFieldList(b []byte) (*FieldList, error) {
	r := &reader{b: b}
	if len(b) == 0 {
		return &FieldList{}, nil
	}
	fl := &FieldList{}
	flags, err := r.u8()
	if err != nil {
		return nil, err
	}
	fl.Flags = flags

	if flags&FieldListHasInfo != 0 {
		infoLen, err := r.u8()
		if err != nil {
			return nil, err
		}
		start := r.pos
		if fl.DictionaryID, err = r.resBitI15(); err != nil {
			return nil, err
		}
		num, err := r.u16()
		if err != nil {
			return nil, err
		}
		fl.FieldListNum = int16(num)
		if start+int(infoLen) > len(b) {
			return nil, ErrIncompleteData
		}
		r.pos = start + int(infoLen) // 按 infoLen 跳过（与 C 一致）
	}

	if flags&FieldListHasSetData != 0 {
		if flags&FieldListHasSetID != 0 {
			if fl.SetID, err = r.resBitU15(); err != nil {
				return nil, err
			}
		}
		if flags&FieldListHasStdData != 0 {
			if _, err = r.buffer15(); err != nil { // encSetData
				return nil, err
			}
		}
		// set-defined 内容需要 set definition db；本 ADS 不下发，直接忽略。
		// 但若要继续读标准条目，需先跳过 encSetData —— 上面已消费。
	}

	if flags&FieldListHasStdData != 0 {
		count, err := r.u16()
		if err != nil {
			return nil, err
		}
		entries, err := r.splitEntries(int(count))
		if err != nil {
			return nil, err
		}
		fl.Entries = entries
	}
	return fl, nil
}

// splitEntries 按 rsslDecodeFieldEntry 的规则切出 count 个条目：
//
//	uint16 fieldId
//	buffer16 payload
func (r *reader) splitEntries(count int) ([]FieldEntry, error) {
	out := make([]FieldEntry, 0, count)
	for i := 0; i < count; i++ {
		fid, err := r.u16()
		if err != nil {
			return nil, err
		}
		payload, err := r.buffer16()
		if err != nil {
			return nil, err
		}
		out = append(out, FieldEntry{FieldID: fid, DataType: DataUnknown, raw: payload})
	}
	return out, nil
}

// DataUnknown RSSL_DT_UNKNOWN：field entry 未带类型，须查字典。
const DataUnknown = 0
