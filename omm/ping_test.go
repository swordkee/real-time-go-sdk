package omm

import (
	"testing"
	"time"
)

// TestPingTimeoutDecoupled 锁住「广告给服务端的超时窗口」与「我方 ping 间隔」的解耦。
//
// 2026-11-23 实测：把 10s 发送间隔同时当作 conn request 的 pingTimeout 广播，
// ADS 会用 10s 空闲窗口与我们 10s 的 ping 赛跑，30 分钟内 FIN 8 次；
// 同期 Java EMA 与 Go ETA SO 版（广告 60s）都是 0 次断线。
// 因此广告窗口必须 >= 60s 且 >= 3×发送间隔。
func TestPingTimeoutDecoupled(t *testing.T) {
	newCfg := func(interval, timeout time.Duration) Config {
		return Config{
			Host: "127.0.0.1", Port: 1, Username: "u",
			DictionaryFile: "../rwf/testdata/RDMFieldDictionary.fixture",
			PingInterval:   interval,
			PingTimeout:    timeout,
		}
	}
	cases := []struct {
		name          string
		interval      time.Duration
		timeout       time.Duration
		wantAdvertise time.Duration
	}{
		{"10s 间隔仍广告 60s", 10 * time.Second, 0, 60 * time.Second},
		{"30s 间隔广告 90s", 30 * time.Second, 0, 90 * time.Second},
		{"显式覆盖优先", 10 * time.Second, 120 * time.Second, 120 * time.Second},
	}
	for _, c := range cases {
		cons, err := NewConsumer(newCfg(c.interval, c.timeout))
		if err != nil {
			t.Fatalf("%s: NewConsumer 失败: %v", c.name, err)
		}
		if cons.cfg.PingInterval != c.interval {
			t.Errorf("%s: PingInterval = %v，期望 %v", c.name, cons.cfg.PingInterval, c.interval)
		}
		nt, ok := cons.tx.(nativeTransport)
		if !ok {
			t.Fatalf("%s: 传输不是 nativeTransport", c.name)
		}
		if got := nt.conn.PingTimeout(); got != c.wantAdvertise {
			t.Errorf("%s: 广告 pingTimeout = %v，期望 %v", c.name, got, c.wantAdvertise)
		}
	}
}
