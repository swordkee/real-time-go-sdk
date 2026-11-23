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
	mu       sync.Mutex
	failN    int
	connects int
	onOK     chan struct{}
	once     sync.Once
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
func (f *reconnectTx) Read() (ReadResult, error) {
	time.Sleep(time.Millisecond)
	return ReadResult{}, nil
}
func (f *reconnectTx) Ping()     {}
func (f *reconnectTx) Shutdown() {}

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
