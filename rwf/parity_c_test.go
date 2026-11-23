package rwf

import (
	"errors"
	"testing"
)

// 本文件是「与 C 返回码/长度集合逐条对齐」的锁。每个 case 都注明 C 源位置；
// 任何把长度集合放宽/收紧、把 BLANK_DATA 当 SUCCESS 的回归都会转红。
// C 参照：Impl/Codec/primitiveDecoders.c（rsslDecodeReal/UInt/Int/Enum/Date/Time/DateTime/Buffer）。

func TestTimeLengthsMatchC(t *testing.T) {
	// rsslDecodeTime: 只接受 0/2/3/5/7/8；1/4/6 与 >8 一律 INCOMPLETE_DATA。
	for _, n := range []int{1, 4, 6, 9, 10} {
		if _, err := DecodeTime(make([]byte, n)); !errors.Is(err, ErrIncompleteData) {
			t.Errorf("TIME len=%d 应为 INCOMPLETE_DATA，实得 %v", n, err)
		}
	}
	for _, n := range []int{2, 3, 5, 7, 8} {
		if _, err := DecodeTime(make([]byte, n)); err != nil {
			t.Errorf("TIME len=%d 应为成功，实得 %v", n, err)
		}
	}
	if _, err := DecodeTime(nil); !errors.Is(err, ErrBlankData) {
		t.Errorf("TIME len=0 应为 BLANK_DATA，实得 %v", err)
	}
}

func TestTimeBlankAndMicroNano(t *testing.T) {
	// 2 字节全 255 → BLANK_DATA，且秒以下按 C 填 255/65535/2047。
	tm, err := DecodeTime([]byte{0xFF, 0xFF})
	if !errors.Is(err, ErrBlankData) {
		t.Fatalf("2 字节全 255 应为 BLANK_DATA，实得 %v", err)
	}
	if tm.Second != 255 || tm.Millisecond != 65535 || tm.Microsecond != 2047 || tm.Nanosecond != 2047 {
		t.Errorf("2 字节 blank 的补齐字段不对: %+v", tm)
	}

	// 8 字节：micro = tempMicro & 0x07FF；nano = ((tempMicro&0x3800)>>3) + tempNano
	// 取 tempMicro = 0x1005（nano 高位=2, micro=5），tempNano=7 → nano = 512+7 = 519。
	p := []byte{1, 2, 3, 0x00, 0x64, 0x10, 0x05, 0x07}
	tm, err = DecodeTime(p)
	if err != nil {
		t.Fatalf("8 字节 TIME 解码失败: %v", err)
	}
	if tm.Hour != 1 || tm.Minute != 2 || tm.Second != 3 || tm.Millisecond != 100 {
		t.Errorf("8 字节 TIME 基本字段不对: %+v", tm)
	}
	if tm.Microsecond != 5 || tm.Nanosecond != 519 {
		t.Errorf("8 字节 TIME micro/nano 解错: micro=%d nano=%d（期望 5/519）", tm.Microsecond, tm.Nanosecond)
	}
}

func TestDateTimeLengthsMatchC(t *testing.T) {
	// rsslDecodeDateTime: 只接受 0/6/7/9/11/12；8/10 等一律 INCOMPLETE_DATA。
	for _, n := range []int{1, 2, 5, 8, 10, 13} {
		if _, err := DecodeDateTime(make([]byte, n)); !errors.Is(err, ErrIncompleteData) {
			t.Errorf("DATETIME len=%d 应为 INCOMPLETE_DATA，实得 %v", n, err)
		}
	}
	for _, n := range []int{6, 7, 9, 11, 12} {
		b := make([]byte, n)
		b[0], b[1] = 23, 11 // day/month 非 0，避免被判 blank
		b[2], b[3] = 0x07, 0xEA
		b[4], b[5] = 6, 45
		if _, err := DecodeDateTime(b); err != nil {
			t.Errorf("DATETIME len=%d 应为成功，实得 %v", n, err)
		}
	}
	if _, err := DecodeDateTime(nil); !errors.Is(err, ErrBlankData) {
		t.Errorf("DATETIME len=0 应为 BLANK_DATA，实得 %v", err)
	}
}

func TestDateTimeBlankMatchesC(t *testing.T) {
	// 12 字节：日期全 0 + 时间全 255/65535/2047 → BLANK_DATA。
	b := []byte{0, 0, 0, 0, 255, 255, 255, 0xFF, 0xFF, 0x10, 0x00, 0x07}
	// tempMicro=0x1000 → micro=0, 加 0x07FF? 需要 micro==2047 且 nano==2047：
	// micro = tempMicro&0x07FF = 2047 → tempMicro 低 11 位全 1；nano = ((tempMicro&0x3800)>>3)+tempNano = 2047
	// 取 tempMicro = 0x3FFF&0x3FFF? 0x3FFF → micro=2047, nano=(0x3800>>3=1792)+tempNano → tempNano=255 → 2047。
	b[9], b[10] = 0x00, 0xFF // tempMicro = 0x00FF → micro=255 不对
	b[9], b[10] = 0x3F, 0xFF // tempMicro = 0x3FFF → micro=2047
	b[11] = 255              // tempNano=255 → nano=1792+255=2047
	dt, err := DecodeDateTime(b)
	if !errors.Is(err, ErrBlankData) {
		t.Fatalf("12 字节全空应为 BLANK_DATA，实得 %v / %+v", err, dt)
	}

	// 时间全空但日期非 0 → C 返回 SUCCESS（不是 blank）。
	b[0] = 23
	if _, err := DecodeDateTime(b); err != nil {
		t.Errorf("日期非 0 时应为 SUCCESS，实得 %v", err)
	}

	// 7 字节时间全 255 + 日期全 0 → BLANK_DATA（秒以下按 C 补 65535/2047/2047）。
	b7 := []byte{0, 0, 0, 0, 255, 255, 255}
	dt, err = DecodeDateTime(b7)
	if !errors.Is(err, ErrBlankData) {
		t.Fatalf("7 字节全空应为 BLANK_DATA，实得 %v", err)
	}
	if dt.Time.Millisecond != 65535 || dt.Time.Microsecond != 2047 || dt.Time.Nanosecond != 2047 {
		t.Errorf("7 字节 blank 的补齐字段不对: %+v", dt.Time)
	}
}

func TestRealHintValidationMatchesC(t *testing.T) {
	// rsslDecodeReal: hint 必须在 [EXPONENT_14(0), NOT_A_NUMBER(35)] 且 != 31/32。
	// 0x1F → 默认分支 hint = 0x1F & 0x1F = 31 → INVALID_DATA。
	if _, err := DecodeReal([]byte{0x1F, 0x00}); !errors.Is(err, ErrInvalidData) {
		t.Errorf("hint=31 应为 INVALID_DATA，实得 %v", err)
	}
	// 0x20|0x1F = 0x3F → 0x3F&0x3F=0x3F 不在 INF 集合 → 默认 hint = 0x1F(31) → INVALID。
	if _, err := DecodeReal([]byte{0x3F, 0x00}); !errors.Is(err, ErrInvalidData) {
		t.Errorf("blank 位 + hint=31 应为 INVALID_DATA，实得 %v", err)
	}
	// hint=0（EXPONENT_14）合法
	if r, err := DecodeReal([]byte{0x00, 0x01}); err != nil || r.Hint != 0 || r.Value != 1 {
		t.Errorf("hint=0 应合法，实得 %+v / %v", r, err)
	}
	// hint=30（FRACTION_256 上界）合法
	if r, err := DecodeReal([]byte{0x1E, 0x01}); err != nil || r.Hint != 30 {
		t.Errorf("hint=30 应合法，实得 %+v / %v", r, err)
	}
	// 单字节 33/34/35 = INF/-INF/NAN 合法
	for _, h := range []byte{RealHintInfinity, RealHintNegInfinity, RealHintNotANumber} {
		if r, err := DecodeReal([]byte{h}); err != nil || r.Blank || r.Hint != h {
			t.Errorf("单字节 hint=%d 应合法，实得 %+v / %v", h, r, err)
		}
	}
	// 长度 10 超过 rwfGetReal64 支持范围 → INVALID_DATA
	if _, err := DecodeReal(make([]byte, 10)); !errors.Is(err, ErrInvalidData) {
		t.Errorf("REAL len=10 应为 INVALID_DATA，实得 %v", err)
	}
}

func TestPrimitiveBlankSemanticsMatchC(t *testing.T) {
	// rsslDecodeUInt/Int/Enum/Buffer：0 长度 → BLANK_DATA
	if _, err := DecodeUInt(nil); !errors.Is(err, ErrBlankData) {
		t.Errorf("UInt 0 长度应为 BLANK_DATA，实得 %v", err)
	}
	if _, err := DecodeInt(nil); !errors.Is(err, ErrBlankData) {
		t.Errorf("Int 0 长度应为 BLANK_DATA，实得 %v", err)
	}
	if _, err := DecodeEnum(nil); !errors.Is(err, ErrBlankData) {
		t.Errorf("Enum 0 长度应为 BLANK_DATA，实得 %v", err)
	}
	if _, err := DecodeBuffer(nil); !errors.Is(err, ErrBlankData) {
		t.Errorf("Buffer 0 长度应为 BLANK_DATA，实得 %v", err)
	}
	// Enum >2 长度：rwfGetLenSpecU16_Size 返回 -1 → INCOMPLETE_DATA（不是 INVALID）
	if _, err := DecodeEnum([]byte{1, 2, 3}); !errors.Is(err, ErrIncompleteData) {
		t.Errorf("Enum len=3 应为 INCOMPLETE_DATA，实得 %v", err)
	}
	// Date 0 长度 → BLANK；全 0 的 4 字节 → BLANK
	if _, err := DecodeDate(nil); !errors.Is(err, ErrBlankData) {
		t.Errorf("Date 0 长度应为 BLANK_DATA，实得 %v", err)
	}
	if _, err := DecodeDate([]byte{0, 0, 0, 0}); !errors.Is(err, ErrBlankData) {
		t.Errorf("Date 全 0 应为 BLANK_DATA，实得 %v", err)
	}
}

// buildMsg 组一条 RWF 消息：2 字节 headerSize + class/domain/stream + 自定义 header 尾。
func buildMsg(class, domain uint8, stream int32, tail []byte) []byte {
	b := []byte{0, 0, class, domain, byte(stream >> 24), byte(stream >> 16), byte(stream >> 8), byte(stream)}
	b = append(b, tail...)
	n := len(b) - 2
	b[0], b[1] = byte(n>>8), byte(n)
	return b
}

func TestMsgKeySizeMustMatchConsumed(t *testing.T) {
	// refresh: flags=HAS_MSG_KEY(0x08), container=0, state(0x09,0,0), groupIdLen=0,
	// keySize=2 但 key 实际消费 3 字节 → C 返回 INVALID_DATA。
	tail := []byte{
		0x08,             // refresh flags
		0x00,             // container NO_DATA
		0x09, 0x00, 0x00, // state: stream=1 data=1, code=0, textLen=0
		0x00,            // groupId length
		0x02,            // keySize = 2
		0x02, 0x01, 'T', // key: HAS_NAME + name "T" → 消费 3 字节
	}
	if _, err := DecodeMsg(buildMsg(MsgClassRefresh, DomainMarketPrice, 3, tail)); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("keySize 不一致应为 INVALID_DATA，实得 %v", err)
	}

	// keySize 正确时应成功
	tail[6] = 0x03
	m, err := DecodeMsg(buildMsg(MsgClassRefresh, DomainMarketPrice, 3, tail))
	if err != nil {
		t.Fatalf("keySize 正确时应成功: %v", err)
	}
	if string(m.Key.Name) != "T" {
		t.Errorf("key name = %q", m.Key.Name)
	}
}

func TestRefreshRequiresGroupIDByteAfterState(t *testing.T) {
	// state 之后没有 groupId 长度字节 → C 返回 INCOMPLETE_DATA。
	tail := []byte{0x00, 0x00, 0x09, 0x00, 0x00}
	if _, err := DecodeMsg(buildMsg(MsgClassRefresh, DomainLogin, 1, tail)); !errors.Is(err, ErrIncompleteData) {
		t.Fatalf("state 后缺 groupId 长度应为 INCOMPLETE_DATA，实得 %v", err)
	}
}

func TestStatusPostUserAndReqKey(t *testing.T) {
	// status flags = STATE|POST_USER_INFO|REQ_MSG_KEY = 0x20|0x400|0x800 = 0xC20（RB15 两字节 0x8C 0x20）
	key := []byte{0x02, 0x01, 'K'} // HAS_NAME "K"
	tail := []byte{
		0x8C, 0x20, // flags
		0x00,             // container
		0x09, 0x00, 0x00, // state
		0x00, 0x00, 0x00, 0x01, // postUserAddr
		0x00, 0x00, 0x00, 0x02, // postUserId
		0x03, // reqMsgKey size
	}
	tail = append(tail, key...)
	m, err := DecodeMsg(buildMsg(MsgClassStatus, DomainMarketPrice, 4, tail))
	if err != nil {
		t.Fatalf("status 解码失败: %v", err)
	}
	if !m.HasPostUser {
		t.Error("POST_USER_INFO 未解出")
	}
	if !m.HasReqKey || string(m.ReqKey.Name) != "K" {
		t.Errorf("REQ_MSG_KEY 未解出: has=%v name=%q", m.HasReqKey, string(m.ReqKey.Name))
	}
}

func TestPostUserInfoGuardUnsetsFlag(t *testing.T) {
	// refresh 声明 HAS_POST_USER_INFO，但 header 里不够 8 字节 → 清标志、不消费 body 字节。
	tail := []byte{
		0x88, 0x00, // flags = HAS_POST_USER_INFO(0x0800)（RB15 两字节）
		0x00,             // container
		0x09, 0x00, 0x00, // state
		0x00, // groupId length
		// 没有 postUserInfo 的 8 字节
	}
	m, err := DecodeMsg(buildMsg(MsgClassRefresh, DomainMarketPrice, 3, tail))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if m.HasPostUser {
		t.Error("header 里没有 postUserInfo 却置了 HasPostUser")
	}
	if m.Flags&RefreshHasPostUserInfo != 0 {
		t.Error("postUserInfo 不存在时应清标志（与 C 一致）")
	}
}

func TestOtherClassesDecode(t *testing.T) {
	// ACK: flags = TEXT|SEQ_NUM|NAK_CODE = 0x02|0x08|0x10 = 0x1A
	tail := []byte{
		0x1A, 0x00,
		0x00, 0x00, 0x00, 0x07, // ackId
		0x02,                // nakCode
		0x03, 'a', 'b', 'c', // text（buffer16）
		0x00, 0x00, 0x00, 0x09, // seqNum
	}
	m, err := DecodeMsg(buildMsg(MsgClassAck, DomainLogin, 5, tail))
	if err != nil {
		t.Fatalf("ACK 解码失败: %v", err)
	}
	if m.AckID != 7 || m.NAKCode != 2 || m.SeqNum != 9 {
		t.Errorf("ACK 字段不对: %+v", m)
	}

	// POST: flags = POST_ID(0x20)；postUserInfo 必备 8 字节
	tail = []byte{
		0x20, 0x00,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x2A, // postId
	}
	m, err = DecodeMsg(buildMsg(MsgClassPost, DomainMarketPrice, 6, tail))
	if err != nil {
		t.Fatalf("POST 解码失败: %v", err)
	}
	if m.PostID != 42 || !m.HasPostUser {
		t.Errorf("POST 字段不对: %+v", m)
	}

	// GENERIC: flags = SEQ_NUM|SECONDARY_SEQ_NUM = 0x10|0x20
	tail = []byte{
		0x30, 0x00,
		0x00, 0x00, 0x00, 0x01, // seq
		0x00, 0x00, 0x00, 0x02, // secondary
	}
	m, err = DecodeMsg(buildMsg(MsgClassGeneric, DomainMarketPrice, 7, tail))
	if err != nil {
		t.Fatalf("GENERIC 解码失败: %v", err)
	}
	if m.SeqNum != 1 || m.SecondarySeqNum != 2 {
		t.Errorf("GENERIC 字段不对: %+v", m)
	}

	// CLOSE
	m, err = DecodeMsg(buildMsg(MsgClassClose, DomainMarketPrice, 3, []byte{0x00, 0x00}))
	if err != nil {
		t.Fatalf("CLOSE 解码失败: %v", err)
	}
	if m.ContainerType != DataNoData {
		t.Errorf("CLOSE container = %d", m.ContainerType)
	}

	// 未知 class → 错误（C: default → FAILURE）
	if _, err := DecodeMsg(buildMsg(9, DomainLogin, 1, []byte{0x00, 0x00})); err == nil {
		t.Error("未知消息类应报错")
	}
}

func TestExtractPropagatesInvalidField(t *testing.T) {
	dict, err := LoadFieldDictionary("testdata/RDMFieldDictionary.fixture")
	if err != nil {
		t.Fatal(err)
	}
	ex := NewQuoteExtractor(dict, []string{"BID"})
	// FIELD_LIST: flags=0x08（仅标准数据，无 info）、count=1、fid=22、payload=非法 REAL(hint 31)
	body := []byte{0x08, 0x00, 0x01, 0x00, 0x16, 0x02, 0x1F, 0x00}
	if _, err := ex.Extract(body); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("非法 REAL 应向上返回 INVALID_DATA，实得 %v", err)
	}
	// 空值字段（0 长度）不应报错，只是不出现。
	body = []byte{0x08, 0x00, 0x01, 0x00, 0x16, 0x00}
	q, err := ex.Extract(body)
	if err != nil {
		t.Fatalf("空值字段不应报错: %v", err)
	}
	if _, ok := q["BID"]; ok {
		t.Errorf("空值字段不应出现: %v", q)
	}
}
