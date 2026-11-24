package omm

import (
	"github.com/swordkee/real-time-go-sdk/native"
)

// nativeTransport 是**纯 Go** 传输（native：RIPC + RWF 编码，零 cgo/零 .so）。
type nativeTransport struct{ conn *native.Conn }

func (t nativeTransport) Connect() error { return t.conn.Connect() }
func (t nativeTransport) Login() error   { return t.conn.Login() }
func (t nativeTransport) Submit(item string, serviceID uint16, streamID int32) error {
	return t.conn.Submit(item, serviceID, streamID)
}

// SubmitItem 一条待提交的订阅请求（批量提交用，屏蔽底层传输类型）。
type SubmitItem struct {
	Item      string
	ServiceID uint16
	StreamID  int32
}

// SubmitBatch 一次提交全部 pending item（native 侧打成单个 packing 帧）。
func (t nativeTransport) SubmitBatch(items []SubmitItem) error {
	ns := make([]native.SubmitItem, 0, len(items))
	for _, it := range items {
		ns = append(ns, native.SubmitItem{Item: it.Item, ServiceID: it.ServiceID, StreamID: it.StreamID})
	}
	return t.conn.SubmitBatch(ns)
}
func (t nativeTransport) Read() (ReadResult, error) {
	r, err := t.conn.Read()
	if err != nil {
		return ReadResult{}, err
	}
	return ReadResult{Data: r.Data, Ret: r.Ret, Ping: r.Ping}, nil
}
func (t nativeTransport) Ping()     { t.conn.Ping() }
func (t nativeTransport) Shutdown() { t.conn.Shutdown() }
