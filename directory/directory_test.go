package directory

import (
	"bytes"
	"errors"
	"testing"

	"github.com/swordkee/real-time-go-sdk/rwf"
)

// 本文件所有 golden 帧都是**手工逐字节构造**的，不调用被测解码器/编码器，
// 每一段都注明对应的 LSEG RTSDK C file:line，防止「自己编自己解」的假绿。

// elemListWithName 构造 SERVICE_INFO 的 ELEMENT_LIST payload。
// C: Impl/Codec/elemListDecoder.c:24-162（header + entry）、elemListEncoder.c:362-370；
// C: Impl/RDM/rsslRDMDirectoryMsg.c:940-945（"Name" 元素、ASCII_STRING/BUFFER）。
func elemListWithName(name string) []byte {
	var b []byte
	b = append(b, elfHasStandardData) // ELF_HAS_STANDARD_DATA(0x08)
	b = append(b, 0, 1)               // u16 itemCount=1（elemListDecoder.c:143）
	b = append(b, 4)                  // buffer15 长度 "Name"=4（resBitU15 单字节）
	b = append(b, []byte(elementNameName)...)
	b = append(b, dtASCIIString)   // dataType=RSSL_DT_ASCII_STRING(17)，不缩放
	b = append(b, byte(len(name))) // buffer16 单字节长度
	b = append(b, []byte(name)...)
	return b
}

// filterListWithServiceInfo 构造 MAP entry 的 FILTER_LIST payload。
// C: Impl/Codec/filterListDecoder.c:13-64（header）、66-146（entry）；
// C: Codec/rtr/rsslRDM.h:493（SERVICE_INFO filter ID=1）。
func filterListWithServiceInfo(name string) []byte {
	el := elemListWithName(name)
	var b []byte
	b = append(b, 0x00)              // flags=0（无 per-entry perm / totalCountHint）
	b = append(b, ctElementListWire) // containerType=ELEMENT_LIST 线上值 5
	b = append(b, 1)                 // count=1
	b = append(b, 0x02)              // action=RSSL_FTEA_SET_ENTRY(2)，高 4 位 flags=0
	b = append(b, serviceInfoFilterID)
	b = append(b, byte(len(el))) // buffer16 长度
	b = append(b, el...)
	return b
}

// mapEntry 构造一条 MAP entry。
// C: Impl/Codec/mapDecoder.c:112-200：action(低4位)/flags(高4位) + [permData]
// + key(buffer15, UInt) + value(buffer16)；DELETE 无 value（:178-187）。
func mapEntry(action uint8, serviceID uint16, filterList []byte) []byte {
	var b []byte
	b = append(b, action) // action 在低 4 位，entry flags=0
	b = append(b, 2)      // buffer15 长度=2（UInt key 两字节大端）
	b = append(b, byte(serviceID>>8), byte(serviceID))
	if action != mapActionDelete {
		b = append(b, byte(len(filterList))) // buffer16 长度
		b = append(b, filterList...)
	}
	return b
}

// serviceMap 构造 directory 消息体的 MAP。
// C: Impl/Codec/mapDecoder.c:14-109；约束见 Impl/RDM/rsslRDMDirectoryMsg.c:852/867
// （keyPrimitiveType=UINT、containerType=FILTER_LIST|NO_DATA）。
func serviceMap(entries ...[]byte) []byte {
	var b []byte
	b = append(b, 0x00)                                      // flags=0
	b = append(b, dtUInt)                                    // keyPrimitiveType=RSSL_DT_UINT(4)
	b = append(b, ctFilterListWire)                          // containerType=FILTER_LIST 线上值 7
	b = append(b, byte(len(entries)>>8), byte(len(entries))) // u16 itemCount
	for _, e := range entries {
		b = append(b, e...)
	}
	return b
}

// refreshFrame 构造完整 RWF REFRESH 帧（带 2 字节 headerSize）。
// C: Impl/Codec/messageDecoders.c decodeRefresh + Impl/RDM/rsslRDMDirectoryMsg.c:850
// （directory REFRESH 体是 MAP）。header = class/domain/streamId/flags/containerType/
// state(OPEN|OK + code + 空 text)/groupId(空 buffer8)。
func refreshFrame(domain uint8, streamID int32, body []byte) []byte {
	var b []byte
	b = append(b, 0, 0) // headerSize 回填
	b = append(b, 2)    // MsgClassRefresh
	b = append(b, domain)
	sid := uint32(streamID)
	b = append(b, byte(sid>>24), byte(sid>>16), byte(sid>>8), byte(sid))
	b = append(b, 0)                // flags u15=0
	b = append(b, 9)                // containerType=MAP 线上值 137-128=9
	b = append(b, 0x09, 0x00, 0x00) // state: StreamState=1(OPEN)<<3|DataState=1(OK)=0x09, code=0, text 空
	b = append(b, 0x00)             // groupId buffer8 长度 0
	putU16At(b, 0, uint16(len(b)-2))
	b = append(b, body...)
	return b
}

// TestGoldenRequest 对照 C 编码器布局的 REQUEST 黄金字节。
// 期望值逐字节来源：rsslInitDefaultRDMDirectoryRequest（rsslRDMDirectoryMsg.h:626-633）
// + DirectoryCallbackClient.cpp:1270-1281 + rsslEncodeMsgInternal key 布局。
func TestGoldenRequest(t *testing.T) {
	got, err := Request(DefaultStreamID)
	if err != nil {
		t.Fatalf("Request 失败: %v", err)
	}
	want := []byte{
		0x00, 0x0f, // headerSize=15
		0x01,                   // msgClass=REQUEST
		0x04,                   // domain=RSSL_DMT_SOURCE(4)（rsslRDM.h:33）
		0x00, 0x00, 0x00, 0x02, // streamId=2（DirectoryCallbackClient.cpp:1270）
		0x04,       // flags=RSSL_RQMF_STREAMING(0x04)
		0x00,       // containerType=NO_DATA
		0x80, 0x05, // keySize RB15=5
		0x08,                   // key flags=RSSL_MKF_HAS_FILTER(0x08)
		0x00, 0x00, 0x00, 0x07, // filter=INFO|STATE|GROUP（rsslRDMDirectoryMsg.h:631）
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Request golden 不符:\n got=%x\nwant=%x", got, want)
	}
	if len(got) != 17 {
		t.Fatalf("Request 长度=%d，期望 17", len(got))
	}
}

// TestGoldenDirectoryRefresh 手工 REFRESH 帧：一条 ADD（serviceId=42, name=TEST_SVC）。
func TestGoldenDirectoryRefresh(t *testing.T) {
	frame := refreshFrame(DomainSource, DefaultStreamID,
		serviceMap(mapEntry(mapActionAdd, 42, filterListWithServiceInfo("TEST_SVC"))))

	got, err := DecodeResponse(frame)
	if err != nil {
		t.Fatalf("DecodeResponse 失败: %v", err)
	}
	want := []Service{{ServiceID: 42, ServiceName: "TEST_SVC", Action: ActionAdd}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("解码结果不符: got=%+v want=%+v", got, want)
	}
}

// TestGoldenDeleteDelivered DELETE 必须交付给上层（清映射），ServiceName 为空。
func TestGoldenDeleteDelivered(t *testing.T) {
	frame := refreshFrame(DomainSource, DefaultStreamID, serviceMap(
		mapEntry(mapActionAdd, 42, filterListWithServiceInfo("TEST_SVC")),
		mapEntry(mapActionDelete, 99, nil),
	))
	got, err := DecodeResponse(frame)
	if err != nil {
		t.Fatalf("DecodeResponse 失败: %v", err)
	}
	want := []Service{
		{ServiceID: 42, ServiceName: "TEST_SVC", Action: ActionAdd},
		{ServiceID: 99, ServiceName: "", Action: ActionDelete},
	}
	if len(got) != len(want) {
		t.Fatalf("条目数不符: got=%+v want=%+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条不符: got=%+v want=%+v", i, got[i], want[i])
		}
	}
}

// TestFailClosedMissingName 有 serviceId/有 payload 但 FILTER_LIST 里没有 SERVICE_INFO
// （只有 STATE filter id=2）→ 必须报错，不能静默返回「有 id 没 name」的 Service。
func TestFailClosedMissingName(t *testing.T) {
	var fl []byte
	el := elemListWithName("TEST_SVC")
	fl = append(fl, 0x00, ctElementListWire, 1) // filter list: flags, container, count
	fl = append(fl, 0x02, 2)                    // action=SET, id=2 (STATE，非 SERVICE_INFO)
	fl = append(fl, byte(len(el)))
	fl = append(fl, el...)

	frame := refreshFrame(DomainSource, DefaultStreamID,
		serviceMap(mapEntry(mapActionAdd, 42, fl)))
	if _, err := DecodeResponse(frame); err == nil {
		t.Fatal("缺 SERVICE_INFO/Name 必须 fail-closed 报错")
	}
}

// TestMutationAction 变异自检 ①：把合法 golden 的 MAP action 从 ADD(2) 改成
// 未知 action(9) → 必须转红；还原后必须恢复绿。
func TestMutationAction(t *testing.T) {
	frame := refreshFrame(DomainSource, DefaultStreamID,
		serviceMap(mapEntry(mapActionAdd, 42, filterListWithServiceInfo("TEST_SVC"))))
	// MAP header: flags/keyType/ct/u16count = 5 字节，per-entry 偏移即帧体起点 + 5。
	bodyOff := len(frame) - len(serviceMap(mapEntry(mapActionAdd, 42, filterListWithServiceInfo("TEST_SVC"))))
	actPos := bodyOff + 5
	if frame[actPos] != mapActionAdd {
		t.Fatalf("mutation 定位错误: actPos=%d 字节=%d", actPos, frame[actPos])
	}
	if _, err := DecodeResponse(frame); err != nil {
		t.Fatalf("变异前 golden 应绿: %v", err)
	}

	orig := frame[actPos]
	frame[actPos] = 9 // 未知 RSSL_MPEA action
	_, err := DecodeResponse(frame)
	frame[actPos] = orig
	if err == nil {
		t.Fatal("改 action 为未知值后必须报错（变异自检失败）")
	}
	if _, err := DecodeResponse(frame); err != nil {
		t.Fatalf("还原后应恢复绿: %v", err)
	}
}

// TestMutationNameLength 变异自检 ②：把 "Name" 元素的 buffer16 长度从 8 改成 0
// → 必须转红；还原后必须恢复绿。
func TestMutationNameLength(t *testing.T) {
	frame := refreshFrame(DomainSource, DefaultStreamID,
		serviceMap(mapEntry(mapActionAdd, 42, filterListWithServiceInfo("TEST_SVC"))))
	nameAt := bytes.Index(frame, []byte("TEST_SVC"))
	if nameAt < 1 {
		t.Fatalf("找不到 name 字节")
	}
	lenPos := nameAt - 1 // buffer16 长度字节
	if frame[lenPos] != byte(len("TEST_SVC")) {
		t.Fatalf("mutation 定位错误: lenPos=%d 字节=%d", lenPos, frame[lenPos])
	}
	base, err := DecodeResponse(frame)
	if err != nil || len(base) != 1 || base[0].ServiceName != "TEST_SVC" {
		t.Fatalf("变异前 golden 应绿且 name=TEST_SVC: got=%+v err=%v", base, err)
	}

	orig := frame[lenPos]
	frame[lenPos] = 0 // 改 name 长度为 0
	_, err = DecodeResponse(frame)
	frame[lenPos] = orig
	if err == nil {
		t.Fatal("改 name 长度后必须报错（变异自检失败）")
	}
	if _, err := DecodeResponse(frame); err != nil {
		t.Fatalf("还原后应恢复绿: %v", err)
	}
}

// TestFailClosedBadDomain 非 directory 域必须拒收。
func TestFailClosedBadDomain(t *testing.T) {
	frame := refreshFrame(rwf.DomainDictionary, DefaultStreamID,
		serviceMap(mapEntry(mapActionAdd, 42, filterListWithServiceInfo("TEST_SVC"))))
	if _, err := DecodeResponse(frame); err == nil {
		t.Fatal("非 SOURCE 域必须 fail-closed")
	}
}

// TestFailClosedTruncated 截断帧必须返回 rwf.ErrIncompleteData（errors.Is 可用）。
func TestFailClosedTruncated(t *testing.T) {
	frame := refreshFrame(DomainSource, DefaultStreamID,
		serviceMap(mapEntry(mapActionAdd, 42, filterListWithServiceInfo("TEST_SVC"))))
	_, err := DecodeResponse(frame[:len(frame)-3])
	if !errors.Is(err, rwf.ErrIncompleteData) {
		t.Fatalf("截断应返回 ErrIncompleteData，实际: %v", err)
	}
}
