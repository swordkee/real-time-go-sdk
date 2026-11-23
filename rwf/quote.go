package rwf

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件把「解出的 RWF 消息 + 字典」组装成报价字段 map，口径对齐 LSEG Java
// MsgHandler.change(OMMMsg) / 经典 SO 版实现的 decodeFields：
//   - 价格（REAL）取 rsslRealToString 的字符串（不二次转数值）；
//   - TIME 取 时:分:秒，DATE 取 yyyy-mm-dd；
//   - DSPLY_NAME 去掉所有空格（Java `replaceAll(" ", "")`）。

// QuoteExtractor 从 FIELD_LIST body 提取报价字段。
type QuoteExtractor struct {
	Dict *FieldDictionary
	// Wanted 关心的字段名；空 = 全部字段（调试用）。
	Wanted map[string]struct{}
}

// NewQuoteExtractor 构造提取器；wanted 为空表示不过滤。
func NewQuoteExtractor(dict *FieldDictionary, wanted []string) *QuoteExtractor {
	e := &QuoteExtractor{Dict: dict}
	if len(wanted) > 0 {
		e.Wanted = make(map[string]struct{}, len(wanted))
		for _, w := range wanted {
			e.Wanted[w] = struct{}{}
		}
	}
	return e
}

func (e *QuoteExtractor) wanted(name string) bool {
	if e == nil || e.Wanted == nil {
		return true
	}
	_, ok := e.Wanted[name]
	return ok
}

// Extract 解 FIELD_LIST 体 → 字段名 → 原文字符串。
//
// 字段条目 dataType 通常是 0（UNKNOWN），此时用字典里的类型解码（与 Java
// `fe.getDataType() == UNKNOWN → fiddef.getOMMType()` 一致）。
//
// 空值（BLANK_DATA：0 长度、或 DATE/TIME 的全 255 形态）按「无该字段」处理；
// 其它解码错误（INVALID_DATA/INCOMPLETE_DATA）原样返回 —— 与 C 的返回码一致，
// 不把坏字段静默当成空值。
func (e *QuoteExtractor) Extract(body []byte) (map[string]string, error) {
	fl, err := DecodeFieldList(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(fl.Entries))
	for _, fe := range fl.Entries {
		def, ok := e.Dict.ByFID(fe.FieldID)
		if !ok {
			continue
		}
		if !e.wanted(def.Name) {
			continue
		}
		dt := fe.DataType
		if dt == DataUnknown {
			dt = def.DataType
		}
		v, present, err := decodeFieldValue(dt, def.Name, fe.raw)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if def.Name == "DSPLY_NAME" {
			v = strings.ReplaceAll(v, " ", "")
		}
		out[def.Name] = v
	}
	return out, nil
}

// decodeFieldValue 按类型解一个字段的「原文」。
// present=false 表示线上是空值（BLANK_DATA），不是错误。
func decodeFieldValue(dt uint8, name string, payload []byte) (string, bool, error) {
	wrap := func(err error) error {
		return fmt.Errorf("字段 %s(type=%d,len=%d): %w", name, dt, len(payload), err)
	}
	switch dt {
	case DataReal:
		r, err := DecodeReal(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		if r.Blank {
			return "", false, nil
		}
		return r.String(), true, nil
	case DataAsciiStr, DataBuffer, DataRmtesStr, DataUTF8Str:
		b, err := DecodeBuffer(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		// RMTES 在本 ADS 上是纯 ASCII（实测 "BROKER       GFX"）；非 ASCII 扩展留待后续（同 Java RFA）。
		return string(b), true, nil
	case DataDate:
		d, err := DecodeDate(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		return d.String(), true, nil
	case DataTime:
		t, err := DecodeTime(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		return t.String(), true, nil
	case DataDateTime:
		d, err := DecodeDateTime(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		if isTimeField(name) {
			return d.Time.String(), true, nil
		}
		if isDateField(name) {
			return d.Date.String(), true, nil
		}
		return "", false, nil
	case DataUInt:
		v, err := DecodeUInt(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		return strconv.FormatUint(v, 10), true, nil
	case DataInt:
		v, err := DecodeInt(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		return strconv.FormatInt(v, 10), true, nil
	case DataEnum:
		v, err := DecodeEnum(payload)
		if err != nil {
			if err == ErrBlankData {
				return "", false, nil
			}
			return "", false, wrap(err)
		}
		return strconv.FormatUint(uint64(v), 10), true, nil
	}
	return "", false, nil
}

// DefaultFields 默认报价字段集合（与 Java DefaultFields / 另一条路的 config.go 同集合）。
var DefaultFields = []string{
	"BID", "ASK", "TRDPRC_1", "MID_PRICE",
	"EXCHTIM", "TIMACT", "TRDTIM_1", "SALTIM", "QUOTIM",
	"QUOTIM_MS", "QUOTE_DATE", "TRADE_DATE", "ACTIV_DATE",
	"MSGTYPE", "DSPLY_NAME",
}

var timeFieldSet = map[string]struct{}{
	"EXCHTIM": {}, "TIMACT": {}, "TRDTIM_1": {}, "SALTIM": {}, "QUOTIM": {},
}

var dateFieldSet = map[string]struct{}{
	"QUOTE_DATE": {}, "TRADE_DATE": {}, "ACTIV_DATE": {},
}

func isTimeField(name string) bool { _, ok := timeFieldSet[name]; return ok }
func isDateField(name string) bool { _, ok := dateFieldSet[name]; return ok }

// Describe 把消息打成一行便于日志/调试。
func Describe(m *Msg) string {
	key := ""
	if m.Key.Name != nil {
		key = string(m.Key.Name)
	}
	state := ""
	if m.State != nil {
		state = fmt.Sprintf(" state=%d/%d text=%q", m.State.StreamState, m.State.DataState, string(m.State.Text))
	}
	return fmt.Sprintf("%s domain=%d stream=%d container=%d keyFlags=0x%x serviceId=%d name=%q%s",
		m.ClassName(), m.Domain, m.StreamID, m.ContainerType, m.Key.Flags, m.Key.ServiceID, key, state)
}
