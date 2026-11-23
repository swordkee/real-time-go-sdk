//go:build eta_live

// 联调用测试（默认不参与门禁：需显式 -tags eta_live + 环境变量）。
//
// 参数只从环境变量读，不硬编码进源码：
//
//	RIPC_LIVE_ADDR=<host>:<port>   （TREP/ADS 地址；不设则 skip）
//	RIPC_LIVE_USER=<DACS 用户名>
//	RIPC_LIVE_SERVICE_ID=<数字 serviceId，订阅测试用>
//	RIPC_LIVE_ITEM=<ric>
//	RIPC_LIVE_SECONDS=15
package ripc

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/swordkee/real-time-go-sdk/rwf"
	"github.com/swordkee/real-time-go-sdk/rwf/encode"
)

func liveConfig(t *testing.T) Config {
	t.Helper()
	addr := os.Getenv("RIPC_LIVE_ADDR")
	if addr == "" {
		t.Skip("未设置 RIPC_LIVE_ADDR，跳过联调测试")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("RIPC_LIVE_ADDR 非法: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.Timeout = 10 * time.Second
	cfg.Logf = t.Logf
	return cfg
}

// TestLiveHandshake 只验证 RIPC 握手（conn request → connack → v14 key exchange）到 ACTIVE。
func TestLiveHandshake(t *testing.T) {
	cfg := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	c, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("RIPC 握手失败: %v", err)
	}
	defer c.Close()
	t.Logf("握手成功 ripc=%d maxUserMsg=%d rwf=%d.%d peerPing=%s",
		c.RipcVersion(), c.MaxUserMsgSize(), c.negotiatedRWF[0], c.negotiatedRWF[1], c.PeerPingTimeout())
	if c.RipcVersion() < 5 || c.MaxUserMsgSize() == 0 {
		t.Fatalf("协商结果异常")
	}
	// 主动 ping 一次，验证写通路。
	if err := c.Ping(); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
}

// TestLiveLoginAndSubscribe 端到端：login → 等登录 refresh → 订阅 item → 收 refresh/update。
func TestLiveLoginAndSubscribe(t *testing.T) {
	cfg := liveConfig(t)
	user := os.Getenv("RIPC_LIVE_USER")
	if user == "" {
		t.Skip("未设置 RIPC_LIVE_USER，跳过登录联调")
	}
	item := os.Getenv("RIPC_LIVE_ITEM")
	if item == "" {
		item = "TEST"
	}
	serviceID := uint16(0)
	if v := os.Getenv("RIPC_LIVE_SERVICE_ID"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("RIPC_LIVE_SERVICE_ID 非法: %v", err)
		}
		serviceID = uint16(n)
	}
	secs := 15
	if v := os.Getenv("RIPC_LIVE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			secs = n
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	c, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("RIPC 握手失败: %v", err)
	}
	defer c.Close()

	login, err := encode.LoginRequest(1, user)
	if err != nil {
		t.Fatalf("编码 login 失败: %v", err)
	}
	if err := c.WriteMessage(login); err != nil {
		t.Fatalf("发送 login 失败: %v", err)
	}
	t.Logf("login 已发送 %d 字节", len(login))

	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	loggedIn := false
	submitted := false
	var refresh, update int
	lastPing := time.Now()
	for time.Now().Before(deadline) {
		if time.Since(lastPing) > 5*time.Second {
			_ = c.Ping()
			lastPing = time.Now()
		}
		data, ping, err := c.Read()
		if err != nil {
			t.Fatalf("读失败: %v", err)
		}
		if ping {
			_ = c.Ping()
			lastPing = time.Now()
			continue
		}
		if data == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		m, derr := rwf.DecodeMsg(data)
		if derr != nil {
			t.Logf("解码失败（%d 字节）: %v", len(data), derr)
			continue
		}
		switch m.Domain {
		case rwf.DomainLogin:
			st := ""
			if m.State != nil {
				st = string(m.State.Text)
			}
			t.Logf("LOGIN 消息 class=%s state=%v text=%q", m.ClassName(), m.State, st)
			if m.Class == rwf.MsgClassRefresh && m.State != nil &&
				m.State.StreamState == rwf.StreamOpen && m.State.DataState == rwf.DataOK {
				loggedIn = true
				if !submitted && serviceID > 0 {
					req, encErr := encode.MarketPriceRequest(3, serviceID, item)
					if encErr != nil {
						t.Fatalf("编码 item request 失败: %v", encErr)
					}
					if err := c.WriteMessage(req); err != nil {
						t.Fatalf("发送 item request 失败: %v", err)
					}
					submitted = true
					t.Logf("item request 已发送 item=%s serviceId=%d streamId=3 (%d 字节)", item, serviceID, len(req))
				}
			}
		case rwf.DomainMarketPrice:
			if m.StreamID != 3 {
				continue
			}
			switch m.Class {
			case rwf.MsgClassRefresh:
				refresh++
			case rwf.MsgClassUpdate:
				update++
			case rwf.MsgClassStatus:
				t.Logf("STATUS stream=3 state=%v", m.State)
			}
			if m.Class == rwf.MsgClassRefresh || m.Class == rwf.MsgClassUpdate {
				// 用官方字典解 BID/ASK 需要字典文件；这里只做报文级证据（hex 前缀 + 长度）。
				t.Logf("%s 收帧 stream=3 len=%d hexPrefix=%x", m.ClassName(), len(data), data[:min(16, len(data))])
			}
		}
		if loggedIn && (refresh > 0 || update > 10) {
			// 收到行情即可提前结束（避免长时间占用连接）。
			break
		}
	}
	if !loggedIn {
		t.Fatal("登录未被接受（未收到 OPEN/OK 的 login refresh）")
	}
	if serviceID > 0 && !submitted {
		t.Fatal("登录后未提交订阅")
	}
	if serviceID > 0 && refresh == 0 && update == 0 {
		t.Fatal("订阅后未收到任何 MARKET_PRICE 行情")
	}
	t.Logf("端到端完成 refresh=%d update=%d", refresh, update)
}
