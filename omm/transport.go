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
func (t nativeTransport) Read() (ReadResult, error) {
	r, err := t.conn.Read()
	if err != nil {
		return ReadResult{}, err
	}
	return ReadResult{Data: r.Data, Ret: r.Ret, Ping: r.Ping}, nil
}
func (t nativeTransport) Ping()     { t.conn.Ping() }
func (t nativeTransport) Shutdown() { t.conn.Shutdown() }
