package rwf

import "fmt"

// 本文件逐条对照 Impl/Codec/messageDecoders.c 的 rsslDecodeMsgHeader / rsslDecodeBaseKey /
// 各 message class 分支。RWF 消息布局（无 RIPC 传输头，即 rsslRead 后 encMsgBuffer 的内容）：
//
//	uint16 headerSize
//	uint8  msgClass   (低 5 位有效，高 3 位保留)
//	uint8  domainType (<128 LSEG 定义域，>=128 用户定义)
//	int32  streamId
//	<headerSize 字节的 class 专属头，含 msgKey 等>
//	<encDataBody：容器体，一直延伸到报文末尾（无长度前缀）>

// MsgClass（rsslMsg.h RsslMsgClasses）。
const (
	MsgClassRequest = 1
	MsgClassRefresh = 2
	MsgClassStatus  = 3
	MsgClassUpdate  = 4
	MsgClassClose   = 5
	MsgClassAck     = 6
	MsgClassGeneric = 7
	MsgClassPost    = 8
)

// 域类型（rsslRDM.h RsslDomainTypes）。
const (
	DomainLogin       = 1
	DomainDictionary  = 5
	DomainMarketPrice = 6
)

// 容器类型（rsslDataTypeEnums.h RsslDataType）。
const (
	ContainerTypeMin = 128
	DataNoData       = 128
	DataFieldList    = 132
	DataElementList  = 133
	DataMap          = 137
)

// MsgKeyFlags（rsslMsgKey.h）。
const (
	MsgKeyHasServiceID = 0x0001
	MsgKeyHasName      = 0x0002
	MsgKeyHasNameType  = 0x0004
	MsgKeyHasFilter    = 0x0008
	MsgKeyHasIDent     = 0x0010
	MsgKeyHasAttrib    = 0x0020
)

// RefreshMsgFlags（rsslRefreshMsg.h）。
const (
	RefreshHasExtendedHeader = 0x0001
	RefreshHasPermData       = 0x0002
	RefreshHasMsgKey         = 0x0008
	RefreshHasSeqNum         = 0x0010
	RefreshSolicited         = 0x0020
	RefreshComplete          = 0x0040
	RefreshHasQos            = 0x0080
	RefreshHasPostUserInfo   = 0x0800
	RefreshHasPartNum        = 0x1000
	RefreshHasReqMsgKey      = 0x2000
)

// UpdateMsgFlags（rsslUpdateMsg.h）。
const (
	UpdateHasExtendedHeader = 0x0001
	UpdateHasPermData       = 0x0002
	UpdateHasMsgKey         = 0x0008
	UpdateHasSeqNum         = 0x0010
	UpdateHasConfInfo       = 0x0020
	UpdateHasPostUserInfo   = 0x0200
)

// StatusMsgFlags（rsslStatusMsg.h）。
const (
	StatusHasExtendedHeader = 0x0001
	StatusHasPermData       = 0x0002
	StatusHasMsgKey         = 0x0008
	StatusHasGroupID        = 0x0010
	StatusHasState          = 0x0020
)

// RequestMsgFlags（rsslRequestMsg.h，ETAC 3.10.1 头文件逐条核对）。
//
// ⚠️ 2026-11-23 修正：旧值是错的（Streaming=0x1/Priority=0x4/Qos=0x8/…），
// 与 C 头文件不符。实测依据：C rsslEncodeMsg 编出的 login request 第 5 字节（flags）是 0x04
// （STREAMING），旧常量下 decoder 会把它当 HAS_PRIORITY 多吃 2 字节、把后续 key 全部解错。
// 之前没暴露是因为本包只解「服务器→客户端」的 message（Refresh/Update/Status），从不解 REQUEST；
// 编码侧（rwf/encode）接入后必须与 C 对齐。
const (
	RequestHasExtendedHeader = 0x0001
	RequestHasPriority       = 0x0002
	RequestStreaming         = 0x0004
	RequestMsgKeyInUpdates   = 0x0008
	RequestConfInfoInUpdates = 0x0010
	RequestNoRefresh         = 0x0020
	RequestHasQos            = 0x0040
	RequestHasWorstQos       = 0x0080
)

// 状态（rsslState.h）。
const (
	StreamOpen = 1
	DataOK     = 1
)

// MsgKey 消息 key（rsslDecodeBaseKey 的结果）。
type MsgKey struct {
	Flags       uint16
	ServiceID   uint16
	NameType    uint8
	Name        []byte
	Filter      uint32
	Identifier  int32
	AttribType  uint8
	AttribBytes []byte
}

// State 消息状态（_rsslDecodeState）。
type State struct {
	StreamState uint8
	DataState   uint8
	Code        uint8
	Text        []byte
}

// Qos 服务质量（_rsslDecodeQos）。
type Qos struct {
	Timeliness uint8
	Rate       uint8
	Dynamic    uint8
	TimeInfo   uint16
	RateInfo   uint16
}

// Msg 解出的一条 RWF 消息（只填纯 Go 侧需要的字段）。
type Msg struct {
	Class         uint8
	Domain        uint8
	ContainerType uint8
	StreamID      int32
	Key           MsgKey

	// 各 class 专属
	Flags     uint16 // refresh/update/status/request 的 flags
	State     *State // refresh / status
	SeqNum    uint32
	GroupID   []byte
	HasQos    bool
	Qos       Qos
	PartNum   uint16
	UpdateTyp uint8 // updateMsg.updateType（0=unsolicited）

	// Body 是 encDataBody（未解析，交给 field list 解码器）。
	Body []byte
	// HeaderLen / BodyLen 便于取证：header 实际消耗字节数（含 2 字节长度）与 body 长度。
	HeaderLen int
}

func (m *Msg) ClassName() string {
	switch m.Class {
	case MsgClassRequest:
		return "REQUEST"
	case MsgClassRefresh:
		return "REFRESH"
	case MsgClassStatus:
		return "STATUS"
	case MsgClassUpdate:
		return "UPDATE"
	case MsgClassClose:
		return "CLOSE"
	case MsgClassAck:
		return "ACK"
	case MsgClassGeneric:
		return "GENERIC"
	case MsgClassPost:
		return "POST"
	}
	return fmt.Sprintf("CLASS(%d)", m.Class)
}

// decodeState 对照 _rsslDecodeState。
func decodeState(r *reader) (State, error) {
	var st State
	v, err := r.u8()
	if err != nil {
		return st, err
	}
	st.DataState = v & 0x07
	st.StreamState = v >> 3
	if st.Code, err = r.u8(); err != nil {
		return st, err
	}
	if st.Text, err = r.buffer15(); err != nil {
		return st, err
	}
	return st, nil
}

// decodeQos 对照 _rsslDecodeQos。
func decodeQos(r *reader) (Qos, error) {
	var q Qos
	v, err := r.u8()
	if err != nil {
		return q, err
	}
	q.Timeliness = v >> 5
	q.Rate = (v >> 1) & 0xF
	q.Dynamic = v & 0x1
	if q.Timeliness > 2 { // > RSSL_QOS_TIME_DELAYED_UNKNOWN
		if q.TimeInfo, err = r.u16(); err != nil {
			return q, err
		}
	}
	if q.Rate > 2 { // > RSSL_QOS_RATE_JIT_CONFLATED
		if q.RateInfo, err = r.u16(); err != nil {
			return q, err
		}
	}
	return q, nil
}

// DecodeKey 对照 rsslDecodeBaseKey：解一个定长 key 块。
func DecodeKey(b []byte) (MsgKey, error) {
	r := &reader{b: b}
	return decodeKey(r)
}

func decodeKey(r *reader) (MsgKey, error) {
	var k MsgKey
	var err error
	if k.Flags, err = r.resBitU15(); err != nil {
		return k, err
	}
	if k.Flags&MsgKeyHasServiceID != 0 {
		if k.ServiceID, err = r.optByteU16(); err != nil {
			return k, err
		}
	}
	if k.Flags&MsgKeyHasName != 0 {
		if k.Name, err = r.buffer8(); err != nil {
			return k, err
		}
		if k.Flags&MsgKeyHasNameType != 0 {
			if k.NameType, err = r.u8(); err != nil {
				return k, err
			}
		}
	}
	if k.Flags&MsgKeyHasFilter != 0 {
		if k.Filter, err = r.u32(); err != nil {
			return k, err
		}
	}
	if k.Flags&MsgKeyHasIDent != 0 {
		v, err := r.u32()
		if err != nil {
			return k, err
		}
		k.Identifier = int32(v)
	}
	if k.Flags&MsgKeyHasAttrib != 0 {
		if k.AttribType, err = r.u8(); err != nil {
			return k, err
		}
		k.AttribType += ContainerTypeMin
		if k.AttribType != DataNoData {
			if k.AttribBytes, err = r.buffer15(); err != nil {
				return k, err
			}
		}
	}
	return k, nil
}

// DecodeMsg 对照 rsslDecodeMsg：解一条完整 RWF 消息（不含 RIPC 传输头）。
func DecodeMsg(b []byte) (*Msg, error) {
	r := &reader{b: b}
	hdrSize, err := r.u16()
	if err != nil {
		return nil, err
	}
	hdrStart := r.pos
	if r.pos+int(hdrSize) > len(b) {
		return nil, ErrIncompleteData
	}
	hdrEnd := hdrStart + int(hdrSize)

	m := &Msg{}
	if m.Class, err = r.u8(); err != nil {
		return nil, err
	}
	m.Class &= 0x1F // 高 3 位保留
	if m.Domain, err = r.u8(); err != nil {
		return nil, err
	}
	sid, err := r.u32()
	if err != nil {
		return nil, err
	}
	m.StreamID = int32(sid)

	switch m.Class {
	case MsgClassRefresh:
		err = decodeRefresh(r, m)
	case MsgClassUpdate:
		err = decodeUpdate(r, m)
	case MsgClassStatus:
		err = decodeStatus(r, m)
	case MsgClassRequest:
		err = decodeRequest(r, m)
	case MsgClassGeneric:
		err = decodeGeneric(r, m)
	default:
		// 其它 class 本 PoC 不解（Close/Ack/Post），body 仍可取出。
		r.pos = hdrEnd
	}
	if err != nil {
		return nil, err
	}

	m.HeaderLen = hdrEnd
	// encDataBody 一直延伸到报文末尾（messageDecoders.c:151）。
	if hdrEnd > len(b) {
		return nil, ErrIncompleteData
	}
	m.Body = b[hdrEnd:]
	return m, nil
}

func decodeRefresh(r *reader, m *Msg) error {
	var err error
	if m.Flags, err = r.resBitU15(); err != nil {
		return err
	}
	ct, err := r.u8()
	if err != nil {
		return err
	}
	m.ContainerType = ct + ContainerTypeMin
	if m.Flags&RefreshHasSeqNum != 0 {
		if m.SeqNum, err = r.u32(); err != nil {
			return err
		}
	}
	st, err := decodeState(r)
	if err != nil {
		return err
	}
	m.State = &st
	if m.GroupID, err = r.buffer8(); err != nil {
		return err
	}
	if m.Flags&RefreshHasPermData != 0 {
		if _, err = r.buffer15(); err != nil {
			return err
		}
	}
	if m.Flags&RefreshHasQos != 0 {
		if m.Qos, err = decodeQos(r); err != nil {
			return err
		}
		m.HasQos = true
	}
	if m.Flags&RefreshHasMsgKey != 0 {
		ks, err := r.resBitU15()
		if err != nil {
			return err
		}
		if err := r.need(int(ks)); err != nil {
			return err
		}
		sub := &reader{b: r.b[r.pos : r.pos+int(ks)]}
		if m.Key, err = decodeKey(sub); err != nil {
			return err
		}
		r.pos += int(ks)
	}
	if m.Flags&RefreshHasExtendedHeader != 0 {
		if _, err = r.buffer8(); err != nil {
			return err
		}
	}
	if m.Flags&RefreshHasPostUserInfo != 0 {
		if _, err = r.u32(); err != nil { // postUserAddr
			return err
		}
		if _, err = r.u32(); err != nil { // postUserId
			return err
		}
	}
	if m.Flags&RefreshHasPartNum != 0 {
		if m.PartNum, err = r.resBitU15(); err != nil {
			return err
		}
	}
	if m.Flags&RefreshHasReqMsgKey != 0 {
		ks, err := r.resBitU15()
		if err != nil {
			return err
		}
		if err := r.need(int(ks)); err != nil {
			return err
		}
		r.pos += int(ks)
	}
	return nil
}

func decodeUpdate(r *reader, m *Msg) error {
	var err error
	if m.Flags, err = r.resBitU15(); err != nil {
		return err
	}
	ct, err := r.u8()
	if err != nil {
		return err
	}
	m.ContainerType = ct + ContainerTypeMin
	if m.UpdateTyp, err = r.u8(); err != nil {
		return err
	}
	if m.Flags&UpdateHasSeqNum != 0 {
		if m.SeqNum, err = r.u32(); err != nil {
			return err
		}
	}
	if m.Flags&UpdateHasConfInfo != 0 {
		if _, err = r.resBitU15(); err != nil { // conflationCount
			return err
		}
		if _, err = r.u16(); err != nil { // conflationTime
			return err
		}
	}
	if m.Flags&UpdateHasPermData != 0 {
		if _, err = r.buffer15(); err != nil {
			return err
		}
	}
	if m.Flags&UpdateHasMsgKey != 0 {
		ks, err := r.resBitU15()
		if err != nil {
			return err
		}
		if err := r.need(int(ks)); err != nil {
			return err
		}
		sub := &reader{b: r.b[r.pos : r.pos+int(ks)]}
		if m.Key, err = decodeKey(sub); err != nil {
			return err
		}
		r.pos += int(ks)
	}
	if m.Flags&UpdateHasExtendedHeader != 0 {
		if _, err = r.buffer8(); err != nil {
			return err
		}
	}
	if m.Flags&UpdateHasPostUserInfo != 0 {
		if _, err = r.u32(); err != nil {
			return err
		}
		if _, err = r.u32(); err != nil {
			return err
		}
	}
	return nil
}

func decodeStatus(r *reader, m *Msg) error {
	var err error
	if m.Flags, err = r.resBitU15(); err != nil {
		return err
	}
	ct, err := r.u8()
	if err != nil {
		return err
	}
	m.ContainerType = ct + ContainerTypeMin
	if m.Flags&StatusHasState != 0 {
		st, err := decodeState(r)
		if err != nil {
			return err
		}
		m.State = &st
	}
	if m.Flags&StatusHasGroupID != 0 {
		if m.GroupID, err = r.buffer8(); err != nil {
			return err
		}
	}
	if m.Flags&StatusHasPermData != 0 {
		if _, err = r.buffer15(); err != nil {
			return err
		}
	}
	if m.Flags&StatusHasMsgKey != 0 {
		ks, err := r.resBitU15()
		if err != nil {
			return err
		}
		if err := r.need(int(ks)); err != nil {
			return err
		}
		sub := &reader{b: r.b[r.pos : r.pos+int(ks)]}
		if m.Key, err = decodeKey(sub); err != nil {
			return err
		}
		r.pos += int(ks)
	}
	if m.Flags&StatusHasExtendedHeader != 0 {
		if _, err = r.buffer8(); err != nil {
			return err
		}
	}
	return nil
}

func decodeRequest(r *reader, m *Msg) error {
	var err error
	if m.Flags, err = r.resBitU15(); err != nil {
		return err
	}
	ct, err := r.u8()
	if err != nil {
		return err
	}
	m.ContainerType = ct + ContainerTypeMin
	if m.Flags&RequestHasPriority != 0 {
		if _, err = r.u8(); err != nil {
			return err
		}
		if _, err = r.optByteU16(); err != nil {
			return err
		}
	}
	if m.Flags&RequestHasQos != 0 {
		if m.Qos, err = decodeQos(r); err != nil {
			return err
		}
		m.HasQos = true
	}
	if m.Flags&RequestHasWorstQos != 0 {
		if _, err = decodeQos(r); err != nil {
			return err
		}
	}
	ks, err := r.resBitU15()
	if err != nil {
		return err
	}
	if err := r.need(int(ks)); err != nil {
		return err
	}
	sub := &reader{b: r.b[r.pos : r.pos+int(ks)]}
	if m.Key, err = decodeKey(sub); err != nil {
		return err
	}
	r.pos += int(ks)
	if m.Flags&RequestHasExtendedHeader != 0 {
		if _, err = r.buffer8(); err != nil {
			return err
		}
	}
	return nil
}

func decodeGeneric(r *reader, m *Msg) error {
	var err error
	if m.Flags, err = r.resBitU15(); err != nil {
		return err
	}
	ct, err := r.u8()
	if err != nil {
		return err
	}
	m.ContainerType = ct + ContainerTypeMin
	// 其余字段本 PoC 不消费
	return nil
}
