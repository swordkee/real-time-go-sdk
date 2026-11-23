package encode

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/swordkee/real-time-go-sdk/rwf"
)

// 黄金 hex 来源（byte-exact 门禁）：用 ETA C 3.10.1 的
// rsslEncodeMsg 现场编码打印（oracle 源码 docs/reference/eta-rwf-enc-oracle.c）：
//
//	LOGIN (user="username")   len=23 00150101000000010400800b0608757365726e616d6501
//	ITEM999 len=22 00140106000000030400800a07fe03e7045445535401
//	ITEM100 len=20 0012010600000003040080080764045445535401
//	ITEM254 len=22 00140106000000030400800a07fe00fe045445535401
//	CLOSE   len=10 00080506000000030000
//
// 任何编码回归（多/少一个字节、flag 位、serviceId 单字节/三字节边界写错）都会让本测试红。
const (
	goldenLogin   = "00150101000000010400800b0608757365726e616d6501"
	goldenItem999 = "00140106000000030400800a07fe03e7045445535401"
	goldenItem100 = "0012010600000003040080080764045445535401"
	goldenItem254 = "00140106000000030400800a07fe00fe045445535401"
	goldenClose   = "00080506000000030000"
)

func mustHex(t *testing.T, got []byte) string {
	t.Helper()
	return hex.EncodeToString(got)
}

// TestGoldenLoginRequest 逐字节对拍 C 侧 login 编码（23 字节）。
func TestGoldenLoginRequest(t *testing.T) {
	b, err := LoginRequest(1, "username")
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if got := mustHex(t, b); got != goldenLogin {
		t.Fatalf("login 编码与 C 不一致:\n got=%s\nwant=%s", got, goldenLogin)
	}
}

// TestGoldenItemRequests serviceId 三档（100 单字节 / 999 三字节 / 254 边界）逐字节对拍。
func TestGoldenItemRequests(t *testing.T) {
	cases := []struct {
		serviceID uint16
		want      string
	}{
		{100, goldenItem100},
		{999, goldenItem999},
		{254, goldenItem254},
	}
	for _, c := range cases {
		b, err := MarketPriceRequest(3, c.serviceID, "TEST")
		if err != nil {
			t.Fatalf("serviceId=%d 编码失败: %v", c.serviceID, err)
		}
		if got := mustHex(t, b); got != c.want {
			t.Fatalf("serviceId=%d 编码与 C 不一致:\n got=%s\nwant=%s", c.serviceID, got, c.want)
		}
	}
	if len(goldenItem100) != 20*2 || len(goldenItem999) != 22*2 {
		t.Fatal("黄金 hex 长度常量自身不一致（测试夹具坏了）")
	}
}

// TestGoldenCloseRequest CLOSE 逐字节对拍（10 字节，无 key）。
func TestGoldenCloseRequest(t *testing.T) {
	if got := mustHex(t, CloseRequest(3, DomainMarketPrice)); got != goldenClose {
		t.Fatalf("close 编码与 C 不一致:\n got=%s\nwant=%s", got, goldenClose)
	}
}

// TestLoginRequestDecodesBack 用 rwf（已被 C 侧报文夹具验证过的解码器）回解自己编的字节：
// 断言 class/domain/streamId/flags/key.name/key.nameType —— 交叉验证编码语义正确。
func TestLoginRequestDecodesBack(t *testing.T) {
	b, err := LoginRequest(1, "username")
	if err != nil {
		t.Fatal(err)
	}
	m, err := rwf.DecodeMsg(b)
	if err != nil {
		t.Fatalf("回解失败: %v", err)
	}
	if m.Class != rwf.MsgClassRequest || m.Domain != rwf.DomainLogin {
		t.Fatalf("class/domain 不对: class=%d domain=%d", m.Class, m.Domain)
	}
	if m.StreamID != 1 {
		t.Fatalf("streamId 应为 1，实际 %d", m.StreamID)
	}
	if m.Flags != rwf.RequestStreaming {
		t.Fatalf("request flags 应为 STREAMING(0x%x)，实际 0x%x", rwf.RequestStreaming, m.Flags)
	}
	if m.Key.Flags != MsgKeyHasName|MsgKeyHasNameType {
		t.Fatalf("key flags 不对: 0x%x", m.Key.Flags)
	}
	if string(m.Key.Name) != "username" || m.Key.NameType != 1 {
		t.Fatalf("key name/nameType 不对: %q/%d", m.Key.Name, m.Key.NameType)
	}
	if m.ContainerType != rwf.DataNoData {
		t.Fatalf("containerType 应为 NO_DATA(128)，实际 %d", m.ContainerType)
	}
}

// TestItemRequestDecodesBack item request 回解：serviceId/name/domain/streamId 全对。
func TestItemRequestDecodesBack(t *testing.T) {
	b, err := MarketPriceRequest(3, 999, "TEST")
	if err != nil {
		t.Fatal(err)
	}
	m, err := rwf.DecodeMsg(b)
	if err != nil {
		t.Fatalf("回解失败: %v", err)
	}
	if m.Domain != rwf.DomainMarketPrice || m.StreamID != 3 {
		t.Fatalf("domain/streamId 不对: %d/%d", m.Domain, m.StreamID)
	}
	if m.Key.ServiceID != 999 || string(m.Key.Name) != "TEST" || m.Key.NameType != 1 {
		t.Fatalf("key 不对: serviceId=%d name=%q type=%d", m.Key.ServiceID, m.Key.Name, m.Key.NameType)
	}
	if len(m.Body) != 0 {
		t.Fatalf("NO_DATA 请求不应有 body，实际 %d 字节", len(m.Body))
	}
}

// TestEncodeErrors fail-closed：空 name / 超长 name 必须报错（不静默发空 key 请求）。
func TestEncodeErrors(t *testing.T) {
	if _, err := LoginRequest(1, ""); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("空 name 应报 ErrEmptyName，实际 %v", err)
	}
	if _, err := MarketPriceRequest(3, 999, strings.Repeat("A", 256)); !errors.Is(err, ErrNameTooLong) {
		t.Fatalf("超长 name 应报 ErrNameTooLong，实际 %v", err)
	}
	// 255 字节是合法上界（buffer8 单字节长度）。
	if _, err := MarketPriceRequest(3, 999, strings.Repeat("A", 255)); err != nil {
		t.Fatalf("255 字节 name 应合法，实际 %v", err)
	}
}

// TestFilterAndIdentifier 覆盖 HAS_FILTER/HAS_IDENTIFIER 两个 optional 字段的编码路径
// （当前 login/item 不用，但 directory 请求会用；回解自证顺序正确）。
func TestFilterAndIdentifier(t *testing.T) {
	b, err := Request(2, rwf.DomainDictionary, MsgKey{
		ServiceID: 42, Name: "TEST_SVC", NameType: 1,
		Filter: 0x1F, HasFilter: true,
		Ident: 7, HasIdent: true,
	}, RequestStreaming)
	if err != nil {
		t.Fatal(err)
	}
	m, err := rwf.DecodeMsg(b)
	if err != nil {
		t.Fatalf("回解失败: %v", err)
	}
	if m.Key.Filter != 0x1F || m.Key.Identifier != 7 {
		t.Fatalf("filter/identifier 不对: %d/%d", m.Key.Filter, m.Key.Identifier)
	}
}
