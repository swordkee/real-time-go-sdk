package omm

import (
	"bufio"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swordkee/real-time-go-sdk/rwf"
)

// loadFrames 读 ../rwf/testdata/rwf_frames.txt（真实 ADS 报文；# 注释）。
// 顺序：0 login refresh / 1 TEST refresh / 2 TEST update / 3 TEST update(小)。
func loadFrames(t *testing.T) [][]byte {
	t.Helper()
	p := filepath.Join("..", "rwf", "testdata", "rwf_frames.txt")
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开报文夹具失败: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("hex 解码失败: %v", err)
		}
		out = append(out, b)
	}
	if len(out) < 3 {
		t.Fatalf("报文夹具条数不足: %d", len(out))
	}
	return out
}

func loadDict(t *testing.T) *rwf.FieldDictionary {
	t.Helper()
	dict, err := rwf.LoadFieldDictionary(filepath.Join("..", "rwf", "testdata", "RDMFieldDictionary.fixture"))
	if err != nil {
		t.Fatalf("加载夹具字典失败: %v", err)
	}
	return dict
}

// TestResolveServiceID 覆盖 serviceId 解析优先级：显式 > ServiceIDs[name] > DefaultServiceID > 报错。
func TestResolveServiceID(t *testing.T) {
	c := &Consumer{cfg: Config{ServiceIDs: map[string]uint16{"TEST_SVC": 42}, DefaultServiceID: 999}}
	cases := []struct {
		name string
		req  ReqMsg
		want uint16
	}{
		{"显式优先", ReqMsg{ServiceName: "TEST_SVC", ServiceID: 100}, 100},
		{"按名解析", ReqMsg{ServiceName: "TEST_SVC"}, 42},
		{"兜底默认", ReqMsg{ServiceName: "UNKNOWN"}, 999},
	}
	for _, tc := range cases {
		got, err := c.resolveServiceID(tc.req)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got=(%d,%v) want %d", tc.name, got, err, tc.want)
		}
	}
	// 无兜底 ⇒ 必须报错（fail-closed）。
	c2 := &Consumer{cfg: Config{}}
	if _, err := c2.resolveServiceID(ReqMsg{ServiceName: "TEST_SVC"}); err == nil {
		t.Fatal("无法解析 serviceId 时必须报错")
	}
}

// TestRegisterClientAssignsStream 校验 registerClient 的句柄递增与 streamId 从 3 起（避让 login1/directory2）。
func TestRegisterClientAssignsStream(t *testing.T) {
	c := &Consumer{cfg: Config{DefaultServiceID: 42}, subs: make(map[int32]*subscription), nextStr: itemStreamBase}
	h1, err := c.RegisterClient(NewReqMsg().Service("TEST_SVC").Item("TEST"), ClientFunc{})
	if err != nil {
		t.Fatalf("注册1失败: %v", err)
	}
	h2, err := c.RegisterClient(NewReqMsg().Service("TEST_SVC").Item("TEST2"), ClientFunc{})
	if err != nil {
		t.Fatalf("注册2失败: %v", err)
	}
	if h2 != h1+1 {
		t.Fatalf("句柄应递增: h1=%d h2=%d", h1, h2)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.subs) != 2 {
		t.Fatalf("订阅数应为 2，实际 %d", len(c.subs))
	}
	if c.subs[3] == nil || c.subs[4] == nil {
		t.Fatalf("streamId 应为 3、4，实际 keys=%v", keys(c.subs))
	}
	if c.subs[3].req.Name != "TEST" {
		t.Fatalf("stream3 应绑定 TEST，实际 %q", c.subs[3].req.Name)
	}
}

func keys(m map[int32]*subscription) []int32 {
	out := make([]int32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestClientFuncNilSafe nil 回调不得 panic。
func TestClientFuncNilSafe(t *testing.T) {
	var f ClientFunc
	f.OnRefreshMsg(Msg{})
	f.OnUpdateMsg(Msg{})
	f.OnStatusMsg(Msg{})
}

// TestDispatchUpdateAndRefresh 用真实 ADS 报文驱动 dispatch：验证 rwf 纯 Go 解码 → omm 回调。
func TestDispatchUpdateAndRefresh(t *testing.T) {
	frames := loadFrames(t)
	dict := loadDict(t)
	extr := rwf.NewQuoteExtractor(dict, []string{"BID", "ASK", "TIMACT"})

	var updated, refreshed int
	var lastUpdate Msg
	client := ClientFunc{
		Update: func(m Msg) {
			updated++
			lastUpdate = m
		},
		Refresh: func(m Msg) { refreshed++ },
	}
	c := &Consumer{
		cfg:  Config{Fields: []string{"BID", "ASK", "TIMACT"}},
		extr: extr,
		subs: map[int32]*subscription{3: {req: ReqMsg{ServiceName: "TEST_SVC", Name: "TEST"}, serviceID: 42, streamID: 3, client: client}},
	}

	// frame[1] = TEST refresh，frame[2] = TEST update
	mr, err := rwf.DecodeMsg(frames[1])
	if err != nil {
		t.Fatalf("refresh 解码失败: %v", err)
	}
	c.dispatch(mr)
	mu, err := rwf.DecodeMsg(frames[2])
	if err != nil {
		t.Fatalf("update 解码失败: %v", err)
	}
	c.dispatch(mu)

	if refreshed != 1 {
		t.Fatalf("应收到 1 次 refresh，实际 %d", refreshed)
	}
	if updated != 1 {
		t.Fatalf("应收到 1 次 update，实际 %d", updated)
	}
	if lastUpdate.Name != "TEST" || lastUpdate.ServiceID != 42 {
		t.Fatalf("update 元数据不对: %+v", lastUpdate)
	}
	if lastUpdate.Fields["BID"] == "" || lastUpdate.Fields["ASK"] == "" {
		t.Fatalf("update 应含 BID/ASK: %v", lastUpdate.Fields)
	}
}

// TestDispatchStatus 直接构造 status 消息，验证 OnStatusMsg 与 State 映射。
func TestDispatchStatus(t *testing.T) {
	var got Msg
	client := ClientFunc{Status: func(m Msg) { got = m }}
	c := &Consumer{
		subs: map[int32]*subscription{3: {req: ReqMsg{Name: "TEST"}, serviceID: 42, streamID: 3, client: client}},
	}
	m := &rwf.Msg{Class: rwf.MsgClassStatus, Domain: rwf.DomainMarketPrice, StreamID: 3,
		State: &rwf.State{StreamState: 4, DataState: 3, Code: 1, Text: []byte("closed")}}
	c.dispatch(m)
	if got.State == nil || got.State.Stream != 4 || got.State.Text != "closed" {
		t.Fatalf("status 映射不对: %+v", got.State)
	}
}
