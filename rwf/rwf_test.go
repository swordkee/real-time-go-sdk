package rwf

import (
	"bufio"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFrames 读 testdata/rwf_frames.txt（真实 ADS 报文，每行一条 hex；# 开头为注释）。
func loadFrames(t *testing.T) [][]byte {
	t.Helper()
	p := filepath.Join("testdata", "rwf_frames.txt")
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开报文夹具 %s 失败: %v", p, err)
	}
	defer func() { _ = f.Close() }()

	var frames [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("报文夹具 hex 非法: %v", err)
		}
		frames = append(frames, b)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读报文夹具失败: %v", err)
	}
	if len(frames) != 4 {
		t.Fatalf("报文夹具应有 4 帧，实际 %d", len(frames))
	}
	return frames
}

// TestDecodeRealFrames 用真实 ADS 报文验证「消息头 + field list + 字典 + 报价字段」全链路。
//
// 期望值全部由 ETA C 库（docs/reference/eta-rwf-dec-oracle.c，同一份 RDMFieldDictionary）解码得出，
// 见 docs/eta-pure-go-poc.md §参照输出。
func TestDecodeRealFrames(t *testing.T) {
	frames := loadFrames(t)

	dict, err := LoadFieldDictionary(filepath.Join("testdata", "RDMFieldDictionary.fixture"))
	if err != nil {
		t.Fatalf("加载字典失败: %v", err)
	}
	if dict.Len() != 8 {
		t.Fatalf("字典条目数 = %d，期望 8（夹具被改了？）", dict.Len())
	}

	// --- 帧 0：login refresh ---
	m0, err := DecodeMsg(frames[0])
	if err != nil {
		t.Fatalf("帧 0 解码失败: %v", err)
	}
	if m0.Class != MsgClassRefresh || m0.Domain != DomainLogin || m0.StreamID != 1 {
		t.Errorf("帧 0 class/domain/stream = %d/%d/%d，期望 2/1/1", m0.Class, m0.Domain, m0.StreamID)
	}
	if m0.ContainerType != DataNoData {
		t.Errorf("帧 0 containerType = %d，期望 128(NO_DATA)", m0.ContainerType)
	}
	if m0.Key.Flags != 0x26 {
		t.Errorf("帧 0 keyFlags = 0x%x，期望 0x26", m0.Key.Flags)
	}
	if got := string(m0.Key.Name); got != "username" {
		t.Errorf("帧 0 login name = %q，期望 username", got)
	}
	if m0.State == nil || m0.State.StreamState != StreamOpen || m0.State.DataState != DataOK {
		t.Errorf("帧 0 state = %+v，期望 Open/Ok", m0.State)
	}
	if got := string(m0.State.Text); got != "Login accepted by host server." {
		t.Errorf("帧 0 state text = %q", got)
	}
	if len(m0.Body) != 0 {
		t.Errorf("帧 0 body 应无数据，实际 %d 字节", len(m0.Body))
	}

	// --- 帧 1：TEST 完整 refresh（239 条 field，含我们要的全部报价字段）---
	m1, err := DecodeMsg(frames[1])
	if err != nil {
		t.Fatalf("帧 1 解码失败: %v", err)
	}
	if m1.Class != MsgClassRefresh || m1.Domain != DomainMarketPrice || m1.StreamID != 3 {
		t.Errorf("帧 1 class/domain/stream = %d/%d/%d，期望 2/6/3", m1.Class, m1.Domain, m1.StreamID)
	}
	if m1.ContainerType != DataFieldList {
		t.Errorf("帧 1 containerType = %d，期望 132(FIELD_LIST)", m1.ContainerType)
	}
	if m1.Key.Flags != 0x7 || m1.Key.ServiceID != 999 || string(m1.Key.Name) != "TEST" {
		t.Errorf("帧 1 key = flags 0x%x serviceId %d name %q，期望 0x7/999/TEST",
			m1.Key.Flags, m1.Key.ServiceID, string(m1.Key.Name))
	}
	if m1.Flags != 0x1fa {
		t.Errorf("帧 1 refreshFlags = 0x%x，期望 0x1fa", m1.Flags)
	}

	fl1, err := DecodeFieldList(m1.Body)
	if err != nil {
		t.Fatalf("帧 1 field list 解码失败: %v", err)
	}
	if fl1.Flags != 0x9 || fl1.DictionaryID != 1 || fl1.FieldListNum != 3 {
		t.Errorf("帧 1 fieldList flags=0x%x dictId=%d listNum=%d，期望 0x9/1/3",
			fl1.Flags, fl1.DictionaryID, fl1.FieldListNum)
	}
	if len(fl1.Entries) != 239 {
		t.Errorf("帧 1 条目数 = %d，期望 239", len(fl1.Entries))
	}
	if fl1.Entries[0].FieldID != 1 {
		t.Errorf("帧 1 首条目 fid = %d，期望 1", fl1.Entries[0].FieldID)
	}
	// 逐字节对拍 field entry payload（hex 由 docs/reference/eta-rwf-dec-oracle.c 的实际输出给出）：
	// 这是最强锁 —— 只要变长编码/长度前缀错一字节，payload hex 就对不上。
	wantPayload := map[uint16]string{
		3:    "42524f4b455220202020202020474658", // DSPLY_NAME "BROKER       GFX"
		5:    "062d",                             // TIMACT 06:45
		17:   "170b07ea",                         // ACTIV_DATE 2026-11-23
		22:   "0a068268",                         // BID hint=10 value=426600
		25:   "0a06845c",                         // ASK hint=10 value=427100
		134:  "0c10ad",                           // MID_PRICE hint=12 value=4269
		3855: "0173926b",                         // QUOTIM_MS 24351339
	}
	seen := make(map[uint16]bool, len(wantPayload))
	for _, fe := range fl1.Entries {
		want, ok := wantPayload[fe.FieldID]
		if !ok {
			continue
		}
		seen[fe.FieldID] = true
		if got := hex.EncodeToString(fe.Raw()); got != want {
			t.Errorf("帧 1 fid=%d payload = %s，期望 %s（变长编码不一致）", fe.FieldID, got, want)
		}
	}
	for fid := range wantPayload {
		if !seen[fid] {
			t.Errorf("帧 1 缺少 fid=%d 的条目", fid)
		}
	}

	ex := NewQuoteExtractor(dict, DefaultFields)
	q1, err := ex.Extract(m1.Body)
	if err != nil {
		t.Fatalf("帧 1 字段提取失败: %v", err)
	}
	want1 := map[string]string{
		"BID":        "42.6600",
		"ASK":        "42.7100",
		"MID_PRICE":  "42.69",
		"TIMACT":     "06:45:00",
		"ACTIV_DATE": "2026-11-23",
		"QUOTIM_MS":  "24351339",
		// Java 口径：DSPLY_NAME 去掉所有空格
		"DSPLY_NAME": "BROKERGFX",
	}
	for k, want := range want1 {
		if got, ok := q1[k]; !ok || got != want {
			t.Errorf("帧 1 %s = %q (present=%v)，期望 %q", k, got, ok, want)
		}
	}

	// --- 帧 2：TEST 增量 update（19 条 field，无 msgKey）---
	m2, err := DecodeMsg(frames[2])
	if err != nil {
		t.Fatalf("帧 2 解码失败: %v", err)
	}
	if m2.Class != MsgClassUpdate || m2.Domain != DomainMarketPrice || m2.StreamID != 3 {
		t.Errorf("帧 2 class/domain/stream = %d/%d/%d，期望 4/6/3", m2.Class, m2.Domain, m2.StreamID)
	}
	if m2.Flags != 0x90 {
		t.Errorf("帧 2 updateFlags = 0x%x，期望 0x90", m2.Flags)
	}
	if m2.SeqNum != 50222 {
		t.Errorf("帧 2 seqNum = %d，期望 50222", m2.SeqNum)
	}
	if m2.Key.Flags != 0 {
		t.Errorf("帧 2 不应带 msgKey，实得 flags=0x%x", m2.Key.Flags)
	}

	q2, err := ex.Extract(m2.Body)
	if err != nil {
		t.Fatalf("帧 2 字段提取失败: %v", err)
	}
	want2 := map[string]string{
		"BID":        "42.6678",
		"ASK":        "42.6978",
		"MID_PRICE":  "42.68",
		"TIMACT":     "06:45:00",
		"ACTIV_DATE": "2026-11-23",
		"QUOTIM_MS":  "24352010",
		"DSPLY_NAME": "HSBCLON",
	}
	for k, want := range want2 {
		if got, ok := q2[k]; !ok || got != want {
			t.Errorf("帧 2 %s = %q (present=%v)，期望 %q", k, got, ok, want)
		}
	}

	// --- 帧 3：只含 BID_1/ASK_1 族（3 条），我们关心的字段应一个都没有 ---
	m3, err := DecodeMsg(frames[3])
	if err != nil {
		t.Fatalf("帧 3 解码失败: %v", err)
	}
	if m3.Class != MsgClassUpdate || m3.SeqNum != 50238 {
		t.Errorf("帧 3 class/seq = %d/%d，期望 4/50238", m3.Class, m3.SeqNum)
	}
	fl3, err := DecodeFieldList(m3.Body)
	if err != nil {
		t.Fatalf("帧 3 field list 解码失败: %v", err)
	}
	if len(fl3.Entries) != 3 {
		t.Errorf("帧 3 条目数 = %d，期望 3", len(fl3.Entries))
	}
	q3, err := ex.Extract(m3.Body)
	if err != nil {
		t.Fatalf("帧 3 字段提取失败: %v", err)
	}
	if len(q3) != 0 {
		t.Errorf("帧 3 不应解出关心字段，实得 %v", q3)
	}
}

// TestRealString 校验 REAL → 字符串转换（含标度/负数/分数/特殊值）。
func TestRealString(t *testing.T) {
	cases := []struct {
		name string
		r    Real
		want string
	}{
		{"exp-4 price", Real{Hint: 10, Value: 426600}, "42.6600"},
		{"exp-2 price", Real{Hint: 12, Value: 4269}, "42.69"},
		{"exp0", Real{Hint: RealHintExponent0, Value: 42}, "42"},
		{"exp+2", Real{Hint: 16, Value: 42}, "4200"},
		{"zero exp+2", Real{Hint: 16, Value: 0}, "0"},
		{"negative", Real{Hint: 10, Value: -426600}, "-42.6600"},
		{"smaller than scale", Real{Hint: 10, Value: 5}, "0.0005"},
		{"fraction 1/2", Real{Hint: RealHintFraction1 + 1, Value: 3}, "1 1/2"},
		{"fraction whole", Real{Hint: RealHintFraction1 + 3, Value: 16}, "2"},
		{"infinity", Real{Hint: RealHintInfinity}, "Inf"},
		{"neg infinity", Real{Hint: RealHintNegInfinity}, "-Inf"},
		{"nan", Real{Hint: RealHintNotANumber}, "NaN"},
		{"blank", Real{Blank: true}, ""},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("%s: Real.String() = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestDecodeRealWire 校验 REAL 的线上编码解析（首字节 format + 大端定点值）。
func TestDecodeRealWire(t *testing.T) {
	// 帧 1 的 BID：hint=10(0x0A), value=426600=0x068268 → 3 字节 format 后 4 字节
	r, err := DecodeReal([]byte{0x0A, 0x00, 0x06, 0x82, 0x68})
	if err != nil {
		t.Fatal(err)
	}
	if r.Blank || r.Hint != 10 || r.Value != 426600 || r.String() != "42.6600" {
		t.Errorf("解出 %+v / %q，期望 hint=10 value=426600 “42.6600”", r, r.String())
	}

	// 负值：REAL 的符号在**定点值的符号位**上（format 只带 hint/blank 位，
	// 见 rwfPutReal64(dptr, sval, fval)：fval 是格式，sval 才是带符号值）。
	// 线上用例取自 ETA C 实测：0A FFF97D98 → -426600 / "-42.6600"。
	r, err = DecodeReal([]byte{0x0A, 0xFF, 0xF9, 0x7D, 0x98})
	if err != nil {
		t.Fatal(err)
	}
	if r.Blank || r.Value != -426600 || r.String() != "-42.6600" {
		t.Errorf("负值解出 %+v / %q", r, r.String())
	}

	// 空 payload → blank
	r, _ = DecodeReal(nil)
	if !r.Blank {
		t.Error("空 payload 应解成 blank")
	}

	// 单字节 INF（hint 33）
	r, _ = DecodeReal([]byte{RealHintInfinity})
	if r.Blank || r.String() != "Inf" {
		t.Errorf("单字节 INF 解出 %+v / %q", r, r.String())
	}
}

// TestWirePrimitives 校验变长原语（ResBitU15 / OptByteU16 / buffer8 / buffer16 /
// LenSpec Size 变体），这些是 RWF 最容易翻错的地方。
func TestWirePrimitives(t *testing.T) {
	// resBitU15：单字节
	r := &reader{b: []byte{0x05, 0xAA}}
	if v, err := r.resBitU15(); err != nil || v != 5 || r.pos != 1 {
		t.Errorf("resBitU15 单字节 = %d,%v pos=%d", v, err, r.pos)
	}
	// resBitU15：0x8005 → 2 字节 0x0005 & 0x7FFF
	r = &reader{b: []byte{0x80, 0x05}}
	if v, err := r.resBitU15(); err != nil || v != 5 || r.pos != 2 {
		t.Errorf("resBitU15 双字节 = %d,%v pos=%d", v, err, r.pos)
	}
	// resBitU15 实际用例：999 → 0x83E7
	r = &reader{b: []byte{0x83, 0xE7}}
	if v, _ := r.resBitU15(); v != 999 {
		t.Errorf("resBitU15(0x83E7) = %d，期望 999", v)
	}

	// resBitI15：单字节负（bit6 置位）
	r = &reader{b: []byte{0x40}}
	if v, err := r.resBitI15(); err != nil || v != -64 {
		t.Errorf("resBitI15(0x40) = %d,%v，期望 -64", v, err)
	}
	// resBitI15：双字节无保留位 → 0x0001
	r = &reader{b: []byte{0x80, 0x01}}
	if v, err := r.resBitI15(); err != nil || v != 1 {
		t.Errorf("resBitI15(0x8001) = %d,%v，期望 1", v, err)
	}

	// optByteU16：<0xFE 单字节
	r = &reader{b: []byte{0x67}}
	if v, _ := r.optByteU16(); v != 0x67 {
		t.Errorf("optByteU16 单字节 = %d", v)
	}
	// optByteU16：0xFE + 2 字节
	r = &reader{b: []byte{0xFE, 0x03, 0xE7}}
	if v, _ := r.optByteU16(); v != 999 {
		t.Errorf("optByteU16 三字节 = %d，期望 999", v)
	}

	// buffer8：长度 0
	r = &reader{b: []byte{0x00}}
	if v, err := r.buffer8(); err != nil || v != nil {
		t.Errorf("buffer8(0) = %v,%v", v, err)
	}
	// buffer8：'TEST' → 0x04 T E S T
	r = &reader{b: []byte{0x04, 'T', 'E', 'S', 'T'}}
	if v, _ := r.buffer8(); string(v) != "TEST" {
		t.Errorf("buffer8 = %q", v)
	}

	// buffer16：短形态
	r = &reader{b: []byte{0x03, 0x01, 0x02, 0x03}}
	if v, _ := r.buffer16(); len(v) != 3 {
		t.Errorf("buffer16 短形态 len=%d", len(v))
	}
	// buffer16：0xFE + 2 字节长度（帧 1 里的 key 值）
	r = &reader{b: []byte{0xFE, 0x00, 0x04, 'T', 'E', 'S', 'T'}}
	if v, _ := r.buffer16(); string(v) != "TEST" {
		t.Errorf("buffer16 长形态 = %q", v)
	}

	// 整数 Size 变体
	if v, _ := DecodeUInt([]byte{0x01, 0x73, 0x92, 0x6B}); v != 24351339 {
		t.Errorf("DecodeUInt = %d，期望 24351339", v)
	}
	// UInt 与 Int 的语义分界：同样 2 字节 0xFFFF，UInt 必须是无符号 65535，
	// Int 必须是 -1（真实现里两者走 beUintSize / beIntSize 两条路）。
	if v, _ := DecodeUInt([]byte{0xFF, 0xFF}); v != 65535 {
		t.Errorf("DecodeUInt(0xFFFF) = %d，期望 65535（UInt 不得做符号扩展）", v)
	}
	if v, _ := DecodeInt([]byte{0xFF, 0xFF}); v != -1 {
		t.Errorf("DecodeInt(0xFFFF) = %d，期望 -1", v)
	}
	if v, _ := DecodeUInt([]byte{0x80, 0x00}); v != 32768 {
		t.Errorf("DecodeUInt(0x8000) = %d，期望 32768", v)
	}
	if v, _ := DecodeInt([]byte{0xFF}); v != -1 {
		t.Errorf("DecodeInt(0xFF) = %d，期望 -1", v)
	}
	if v, _ := DecodeInt([]byte{0x01, 0x00}); v != 256 {
		t.Errorf("DecodeInt(0x0100) = %d，期望 256", v)
	}
}

// TestDecodeTimeDate 校验 TIME / DATE 编码。
func TestDecodeTimeDate(t *testing.T) {
	tm, err := DecodeTime([]byte{0x06, 0x2D, 0x00})
	if err != nil || tm.String() != "06:45:00" {
		t.Errorf("TIME = %v,%v", tm, err)
	}
	d, err := DecodeDate([]byte{0x17, 0x0B, 0x07, 0xEA}) // day=23 month=11 year=2026
	if err != nil || d.String() != "2026-11-23" {
		t.Errorf("DATE = %v,%v", d, err)
	}
}

// TestTruncatedFrame 校验截断报文 fail-closed（不 panic、报错）。
//
// 注意与 C 同构：encDataBody **没有长度前缀**，一直延伸到报文末尾
// （messageDecoders.c:151 "the data section is not length specified"），
// 所以只截 body 时 rsslDecodeMsg 仍会成功 —— 真正的截断由 field list 解码暴露。
// 这里断言「消息头 + 字段表 + 取值」整链必须失败。
func TestTruncatedFrame(t *testing.T) {
	frames := loadFrames(t)
	dict, err := LoadFieldDictionary(filepath.Join("testdata", "RDMFieldDictionary.fixture"))
	if err != nil {
		t.Fatalf("加载字典失败: %v", err)
	}
	ex := NewQuoteExtractor(dict, DefaultFields)

	full := frames[1]
	for _, n := range []int{0, 1, 2, 3, 5, 10, 50, len(full) / 2, len(full) - 1} {
		ok := false
		if m, err := DecodeMsg(full[:n]); err == nil {
			if _, err := ex.Extract(m.Body); err == nil {
				ok = true
			}
		}
		if ok {
			t.Errorf("截断到 %d 字节整链仍成功（应报错）", n)
		}
	}

	// 头部本身截断必须直接报 INCOMPLETE_DATA（headerSize 声明 0x0027=39 字节）
	if _, err := DecodeMsg(full[:4]); err == nil {
		t.Error("头部截断到 4 字节仍解码成功（应报错）")
	}
}
