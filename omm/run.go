package omm

import (
	"context"
	"fmt"
	"time"

	"github.com/swordkee/real-time-go-sdk/rwf"
)

// Run 启动 EMA 消费循环（阻塞直到 ctx 取消）：
//
//	connect → login → (login refresh OK) → 提交全部订阅 → 读循环 dispatch
//	读出错/通道断开 → 退避重连 → 登录后重新提交订阅（watchlist 语义）
//
// 单条消息解码失败只记日志继续（与 Java `catch(Throwable)` 后继续跑一致）。
func (c *Consumer) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return fmt.Errorf("omm: Consumer 已在运行，不能重复 Run")
	}
	c.started = true
	tx := c.tx
	c.mu.Unlock()

	defer tx.Shutdown()

	backoff := c.cfg.ReconnectBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := c.runOnce(ctx, tx)
		if ctx.Err() != nil {
			return nil
		}
		c.logger().Error("ETA-EMA 连接中断，准备重连", "err", err, "backoff", backoff.String())
		tx.Shutdown() // 关连接；下一轮 Connect 会重新建/握手
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// runOnce 一次完整的连接生命周期（连接→登录→订阅→读到断/错）。
func (c *Consumer) runOnce(ctx context.Context, tx Transport) error {
	// 重连后所有订阅都要重发（watchlist 语义），先把提交标记清掉。
	c.resetSubmitted()

	if err := tx.Connect(); err != nil {
		return err
	}
	c.logger().Info("ETA-EMA channel 已连接", "host", c.cfg.Host, "port", c.cfg.Port,
		"subs", c.subCount())
	if err := tx.Login(); err != nil {
		return err
	}

	loggedIn := false
	lastPing := time.Now()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(lastPing) >= c.cfg.PingInterval {
			tx.Ping()
			lastPing = time.Now()
		}

		res, err := tx.Read()
		if err != nil {
			return err
		}
		if res.Ping {
			tx.Ping()
			lastPing = time.Now()
			continue
		}
		if len(res.Data) == 0 {
			// 无消息：让出 CPU（rsslRead 非阻塞）。
			time.Sleep(time.Millisecond)
			// 登录后仍可能有「Run 期间新注册」的订阅，顺手补发。
			if loggedIn {
				if err := c.submitPending(tx); err != nil {
					return err
				}
			}
			continue
		}

		m, derr := rwf.DecodeMsg(res.Data)
		if derr != nil {
			c.logger().Warn("RWF 解码失败", "err", derr, "len", len(res.Data))
			continue
		}

		switch m.Domain {
		case rwf.DomainLogin:
			done, err := c.handleLogin(m)
			if err != nil {
				return err
			}
			if done && !loggedIn {
				loggedIn = true
				c.logger().Info("ETA-EMA 登录已接受，提交订阅")
				if err := c.submitPending(tx); err != nil {
					return err
				}
			}
		case rwf.DomainMarketPrice:
			c.dispatch(m)
		}
	}
}

// handleLogin 处理 login 域的 refresh/status。done=登录被接受。
func (c *Consumer) handleLogin(m *rwf.Msg) (done bool, err error) {
	switch m.Class {
	case rwf.MsgClassRefresh:
		if m.State == nil {
			return false, nil
		}
		ok := m.State.StreamState == rwf.StreamOpen && m.State.DataState == rwf.DataOK
		c.logger().Info("ETA-EMA 登录响应", "streamState", m.State.StreamState,
			"dataState", m.State.DataState, "text", string(m.State.Text))
		if !ok {
			return false, fmt.Errorf("omm: 登录 refresh 非 OPEN/OK (stream=%d data=%d text=%q)",
				m.State.StreamState, m.State.DataState, string(m.State.Text))
		}
		return true, nil
	case rwf.MsgClassStatus:
		st := "nil"
		if m.State != nil {
			st = fmt.Sprintf("stream=%d data=%d code=%d text=%q", m.State.StreamState, m.State.DataState, m.State.Code, string(m.State.Text))
		}
		return false, fmt.Errorf("omm: 登录被拒（%s）", st)
	}
	return false, nil
}

// dispatch 把一条 MARKET_PRICE 消息分派给对应客户端回调。
func (c *Consumer) dispatch(m *rwf.Msg) {
	c.mu.Lock()
	sub, ok := c.subs[m.StreamID]
	c.mu.Unlock()
	if !ok {
		return // 未知 stream（可能是 directory/其它域），忽略
	}
	msg := Msg{
		Class:     m.Class,
		Domain:    m.Domain,
		StreamID:  m.StreamID,
		ServiceID: sub.serviceID,
		Name:      sub.req.Name,
		At:        time.Now(),
	}
	if m.State != nil {
		msg.State = &State{
			Stream: m.State.StreamState,
			Data:   m.State.DataState,
			Code:   m.State.Code,
			Text:   string(m.State.Text),
		}
	}
	switch m.Class {
	case rwf.MsgClassRefresh, rwf.MsgClassUpdate:
		if fields, err := c.extr.Extract(m.Body); err == nil {
			msg.Fields = fields
		} else {
			c.logger().Warn("字段解码失败", "err", err, "stream", m.StreamID)
		}
	}

	switch m.Class {
	case rwf.MsgClassRefresh:
		sub.client.OnRefreshMsg(msg)
	case rwf.MsgClassUpdate:
		sub.client.OnUpdateMsg(msg)
	case rwf.MsgClassStatus:
		sub.client.OnStatusMsg(msg)
	}
}

// submitPending 提交所有「登录后尚未发过 item request」的订阅。
func (c *Consumer) submitPending(tx Transport) error {
	c.mu.Lock()
	type item struct {
		name   string
		svc    uint16
		stream int32
		sub    *subscription
	}
	var pending []item
	for _, sid := range c.order {
		s := c.subs[sid]
		if s == nil || s.submitted {
			continue
		}
		pending = append(pending, item{s.req.Name, s.serviceID, s.streamID, s})
	}
	c.mu.Unlock()

	for _, it := range pending {
		if err := tx.Submit(it.name, it.svc, it.stream); err != nil {
			return fmt.Errorf("omm: 提交订阅 %q 失败: %w", it.name, err)
		}
		c.logger().Info("ETA-EMA item request 已发送", "item", it.name, "serviceId", it.svc, "streamId", it.stream)
		c.mu.Lock()
		it.sub.submitted = true
		c.mu.Unlock()
	}
	return nil
}

// resetSubmitted 重连前清掉提交标记（触发重新订阅）。
func (c *Consumer) resetSubmitted() {
	c.mu.Lock()
	for _, s := range c.subs {
		s.submitted = false
	}
	c.mu.Unlock()
}

// subCount 订阅数（日志用）。
func (c *Consumer) subCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.subs)
}
