package rwf

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// 本文件解析 RDMFieldDictionary（FID → 字段名 + RWF 类型），口径与 LSEG Java RFA
// FieldDictionary 一致。
//
// 文件格式（注释行以 '!' 开头）：
//
//	ACRONYM    "DESCRIPTION"        FID  RIPPLES TO  FIELD TYPE  LENGTH  RWF TYPE  RWF LEN
//	BID        "BID"                 22  BID_1       PRICE         17  REAL64        7
//	TIMACT     "TIME OF UPDATE"       5  NULL        TIME           5  TIME          5
//
// 描述列带引号且内含空格，不能按空白切：取首个引号前的名字，闭合引号后的
// 第 1 个十进制字段为 FID、第 5 个字段为 RWF 类型。

// FieldDef 一个字段定义。
type FieldDef struct {
	FID      uint16
	Name     string
	DataType uint8 // RWF 数据类型（DataReal / DataTime / ...）
}

// FieldDictionary FID → 定义（也保留 name → FID 反向索引，便于订阅时构造 fid 列表）。
type FieldDictionary struct {
	byFID  map[uint16]FieldDef
	byName map[string]uint16
}

// Len 字典条目数。
func (d *FieldDictionary) Len() int {
	if d == nil {
		return 0
	}
	return len(d.byFID)
}

// ByFID 查字段定义。
func (d *FieldDictionary) ByFID(fid uint16) (FieldDef, bool) {
	if d == nil {
		return FieldDef{}, false
	}
	v, ok := d.byFID[fid]
	return v, ok
}

// FIDByName 查字段 FID。
func (d *FieldDictionary) FIDByName(name string) (uint16, bool) {
	if d == nil {
		return 0, false
	}
	v, ok := d.byName[name]
	return v, ok
}

// LoadFieldDictionary 读整个 RDMFieldDictionary（2000+ 条，常驻内存约几十 KB，可接受）。
func LoadFieldDictionary(path string) (*FieldDictionary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("rwf: 打开 RDM 字段字典 %s 失败: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	d := &FieldDictionary{
		byFID:  make(map[uint16]FieldDef, 4096),
		byName: make(map[string]uint16, 4096),
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "!") || strings.HasPrefix(t, "#") {
			continue
		}
		name, fid, rwfType, ok := parseDictLine(line)
		if !ok {
			continue
		}
		def := FieldDef{FID: fid, Name: name, DataType: rwfTypeToDataType(rwfType)}
		d.byFID[fid] = def
		if _, exists := d.byName[name]; !exists {
			d.byName[name] = fid
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("rwf: 读取 RDM 字段字典 %s 失败: %w", path, err)
	}
	if len(d.byFID) == 0 {
		return nil, fmt.Errorf("rwf: RDM 字段字典 %s 里没有解析出任何字段（格式不符？）", path)
	}
	return d, nil
}

func parseDictLine(line string) (name string, fid uint16, rwfType string, ok bool) {
	q := strings.IndexByte(line, '"')
	if q <= 0 {
		return "", 0, "", false
	}
	name = strings.TrimSpace(line[:q])
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", 0, "", false
	}
	rest := line[q+1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", 0, "", false
	}
	fields := strings.Fields(rest[end+1:])
	if len(fields) < 5 {
		return "", 0, "", false
	}
	v, err := strconv.ParseUint(fields[0], 10, 16)
	if err != nil {
		return "", 0, "", false
	}
	return name, uint16(v), fields[4], true
}

// rwfTypeToDataType 字典 RWF 类型名 → RWF 数据类型枚举。
func rwfTypeToDataType(t string) uint8 {
	switch t {
	case "REAL64", "REAL", "DOUBLE", "FLOAT":
		return DataReal
	case "TIME", "TIME_SECONDS":
		return DataTime
	case "DATE":
		return DataDate
	case "DATETIME":
		return DataDateTime
	case "UINT64", "UINT", "UINT32":
		return DataUInt
	case "INT64", "INT", "INT32":
		return DataInt
	case "ENUM":
		return DataEnum
	case "RMTES_STRING":
		return DataRmtesStr
	case "ASCII_STRING", "STRING", "UTF8_STRING":
		return DataAsciiStr
	case "BUFFER":
		return DataBuffer
	}
	return DataUnknown
}
