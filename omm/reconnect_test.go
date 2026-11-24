package omm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// reconnectTx 是失败注入用的假传输：前 failN 次 Connect 失败，之后成功并通知测试取消 ctx。
type reconnectTx struct {
	mu        sync.Mutex
	failN     int
	connects  int
	onOK      chan struct{}
	once      sync.Once
	batches   [][]SubmitItem
	submitErr error
}

func (f *reconnectTx) Connect() error {
	f.mu.Lock()
	f.connects++
	n := f.connects
	f.mu.Unlock()
	if n <= f.failN {
		return errors.New("injected connect failure")
	}
	f.once.Do(func() { close(f.onOK) })
	return nil
}
func (f *reconnectTx) Login() error                       { return nil }
func (f *reconnectTx) Submit(string, uint16, int32) error { return nil }
func (f *reconnectTx) SubmitBatch(items []SubmitItem) error {
	f.mu.Lock()
	f.batches = append(f.batches, items)
	err := f.submitErr
	f.mu.Unlock()
	return err
}
func (f *reconnectTx) Read() (ReadResult, error) {
	time.Sleep(time.Millisecond)
	return ReadResult{}, nil
}
func (f *reconnectTx) Ping()     {}
func (f *reconnectTx) Shutdown() {}

// TestSubmitPendingBatchesAllItems 锁住 watchlist 语义：一次性批量提交全部 pending item，
// 成功后全部标记 submitted，重复调用不再产生第二批（避免 poll 循环重复订阅）。
func TestSubmitPendingBatchesAllItems(t *testing.T) {
	tx := &reconnectTx{}
	c := &Consumer{
		cfg:     Config{ServiceIDs: map[string]uint16{"SVC": 7}},
		subs:    make(map[int32]*subscription),
		nextStr: itemStreamBase,
		tx:      tx,
	}
	if _, err := c.RegisterClient(NewReqMsg().Service("SVC").Item("A"), ClientFunc{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterClient(NewReqMsg().Service("SVC").Item("B"), ClientFunc{}); err != nil {
		t.Fatal(err)
	}
	if err := c.submitPending(tx); err != nil {
		t.Fatalf("submitPending: %v", err)
	}
	tx.mu.Lock()
	if len(tx.batches) != 1 || len(tx.batches[0]) != 2 {
		t.Fatalf("应一次提交 2 条，实际 batches=%d len=%v", len(tx.batches), batchLens(tx.batches))
	}
	b := tx.batches[0]
	tx.mu.Unlock()
	if b[0].Item != "A" || b[1].Item != "B" || b[0].ServiceID != 7 || b[0].StreamID != 3 || b[1].StreamID != 4 {
		t.Fatalf("批量内容不对: %+v", b)
	}
	c.mu.Lock()
	for _, sid := range []int32{3, 4} {
		if !c.subs[sid].submitted {
			t.Fatalf("stream %d 应标记 submitted", sid)
		}
	}
	c.mu.Unlock()

	if err := c.submitPending(tx); err != nil {
		t.Fatalf("二次 submitPending: %v", err)
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if len(tx.batches) != 1 {
		t.Fatalf("无新增 pending 时不得再提交，实际 batches=%d", len(tx.batches))
	}
}

// TestSubmitPendingPropagatesError 批量提交失败不得静默吞错：必须原样返回给 Run 走重连。
func TestSubmitPendingPropagatesError(t *testing.T) {
	tx := &reconnectTx{submitErr: errors.New("inject batch failure")}
	c := &Consumer{
		cfg:     Config{ServiceIDs: map[string]uint16{"SVC": 7}},
		subs:    make(map[int32]*subscription),
		nextStr: itemStreamBase,
		tx:      tx,
	}
	if _, err := c.RegisterClient(NewReqMsg().Service("SVC").Item("A"), ClientFunc{}); err != nil {
		t.Fatal(err)
	}
	if err := c.submitPending(tx); err == nil {
		t.Fatal("批量提交失败时 submitPending 必须返回错误")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subs[3].submitted {
		t.Fatal("批量提交失败时不得标记 submitted（否则重连后不会重发）")
	}
}

func batchLens(bs [][]SubmitItem) []int {
	out := make([]int, 0, len(bs))
	for _, b := range bs {
		out = append(out, len(b))
	}
	return out
}

// TestRunReconnectsAfterConnectFailure 破坏性锁：Connect 失败不能让 Run 直接退出，
// 必须按退避重试；这是隧道/ADS 断链后能自动恢复的前提。
func TestRunReconnectsAfterConnectFailure(t *testing.T) {
	tx := &reconnectTx{failN: 2, onOK: make(chan struct{})}
	c := &Consumer{
		cfg: Config{
			ReconnectBackoff: time.Millisecond,
			PingInterval:     time.Hour,
			ServiceIDs:       map[string]uint16{"SVC": 1},
		},
		subs: make(map[int32]*subscription),
		tx:   tx,
	}
	if _, err := c.RegisterClient(NewReqMsg().Service("SVC").Item("X"), ClientFunc{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		<-tx.onOK // 第一次成功后立刻结束，避免等待 login refresh
		cancel()
	}()
	if err := c.Run(ctx); err != nil {
		t.Fatalf("重连流程不应返回错误: %v", err)
	}
	tx.mu.Lock()
	got := tx.connects
	tx.mu.Unlock()
	if got < 3 {
		t.Fatalf("Connect 失败 2 次后应重试到第 3 次，实际调用 %d 次", got)
	}
}
