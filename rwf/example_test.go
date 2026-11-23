package rwf

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// officialDictionaryCandidates 官方 RDMFieldDictionary（LSEG 完整版，2000+ 条）的查找位置。
// 单元测试环境里不一定存在；存在才跑完整字典断言，否则跳过（夹具测试已覆盖解析逻辑）。
var officialDictionaryCandidates = []string{
	os.Getenv("RDM_FIELD_DICTIONARY"),
	filepath.Join("testdata", "RDMFieldDictionary"),
}

// TestOfficialDictionary 用 LSEG 官方字典（2000+ 条）验证解析器，而不是只用裁剪夹具。
func TestOfficialDictionary(t *testing.T) {
	var path string
	for _, p := range officialDictionaryCandidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			path = p
			break
		}
	}
	if path == "" {
		t.Skip("找不到官方 RDMFieldDictionary，跳过（夹具测试已覆盖解析逻辑）")
	}
	d, err := LoadFieldDictionary(path)
	if err != nil {
		t.Fatalf("解析官方字典失败: %v", err)
	}
	t.Logf("官方字典 %s 解出 %d 个字段", path, d.Len())
	if d.Len() < 2000 {
		t.Errorf("官方字典条目数 = %d，远小于预期（解析漏行？）", d.Len())
	}
	for _, c := range []struct {
		name string
		fid  uint16
		typ  uint8
	}{
		{"BID", 22, DataReal},
		{"ASK", 25, DataReal},
		{"TIMACT", 5, DataTime},
		{"DSPLY_NAME", 3, DataRmtesStr},
		{"ACTIV_DATE", 17, DataDate},
		{"QUOTIM_MS", 3855, DataUInt},
		{"MID_PRICE", 134, DataReal},
		{"TRDPRC_1", 6, DataReal},
	} {
		def, ok := d.ByFID(c.fid)
		if !ok {
			t.Errorf("官方字典缺少 fid=%d (%s)", c.fid, c.name)
			continue
		}
		if def.Name != c.name || def.DataType != c.typ {
			t.Errorf("fid=%d = {%s, %d}，期望 {%s, %d}", c.fid, def.Name, def.DataType, c.name, c.typ)
		}
		if fid, ok := d.FIDByName(c.name); !ok || fid != c.fid {
			t.Errorf("FIDByName(%s) = %d,%v，期望 %d", c.name, fid, ok, c.fid)
		}
	}

	// 用官方字典跑一遍真实报文，结果必须与夹具一致（同一份数据、同一套字段名）。
	frames := loadFrames(t)
	m, err := DecodeMsg(frames[1])
	if err != nil {
		t.Fatal(err)
	}
	q, err := NewQuoteExtractor(d, DefaultFields).Extract(m.Body)
	if err != nil {
		t.Fatal(err)
	}
	if q["BID"] != "42.6600" || q["ASK"] != "42.7100" || q["TIMACT"] != "06:45:00" {
		t.Errorf("官方字典下 BID/ASK/TIMACT = %q/%q/%q", q["BID"], q["ASK"], q["TIMACT"])
	}
}

// ExampleDecodeMsg 展示最小用法：hex 报文 → 消息头 → 字段表 → 报价字段。
// 期望输出见函数末尾的 Output 注释（go test 会逐字节比对）。
func ExampleDecodeMsg() {
	f, err := os.Open(filepath.Join("testdata", "rwf_frames.txt"))
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer func() { _ = f.Close() }()

	var raw []byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			fmt.Println("hex:", err)
			return
		}
		raw = append(raw, b...)
		if m, err := DecodeMsg(raw); err == nil && m.Domain == DomainMarketPrice {
			break // 第一条行情报文
		}
		raw = raw[:0] // 不是完整行情报文，继续拼
	}

	dict, err := LoadFieldDictionary(filepath.Join("testdata", "RDMFieldDictionary.fixture"))
	if err != nil {
		fmt.Println("dict:", err)
		return
	}
	m, err := DecodeMsg(raw)
	if err != nil {
		fmt.Println("decode:", err)
		return
	}
	q, err := NewQuoteExtractor(dict, []string{"BID", "ASK", "TIMACT"}).Extract(m.Body)
	if err != nil {
		fmt.Println("extract:", err)
		return
	}
	fmt.Printf("%s stream=%d name=%s\n", m.ClassName(), m.StreamID, string(m.Key.Name))
	fmt.Printf("BID=%s ASK=%s TIMACT=%s\n", q["BID"], q["ASK"], q["TIMACT"])
	// Output:
	// REFRESH stream=3 name=TEST
	// BID=42.6600 ASK=42.7100 TIMACT=06:45:00
}
