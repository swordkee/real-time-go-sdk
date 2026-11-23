// Package fields 是 ETA/MARKET_PRICE 字段名字面值（与 LSEG 参考实现中
// reuters.Fields / MsgHandler 一致的精确匹配名）的唯一来源。
//
// 单独成包的原因：omm（纯 Go EMA）只用这一组字符串；把字段名抽出来后，
// 纯 Go 路径（omm → native → ripc/rwf）就不必依赖任何传输/闭源库相关代码。
package fields

// 字段名字面值。
const (
	FieldBid       = "BID"
	FieldAsk       = "ASK"
	FieldTrdPrc1   = "TRDPRC_1"
	FieldMidPrice  = "MID_PRICE"
	FieldDsplyName = "DSPLY_NAME"
	FieldTimAct    = "TIMACT"
	FieldExchTim   = "EXCHTIM"
	FieldTrdTim1   = "TRDTIM_1"
	FieldSalTim    = "SALTIM"
	FieldQuotim    = "QUOTIM"
	FieldQuotimMs  = "QUOTIM_MS"
	FieldQuoteDate = "QUOTE_DATE"
	FieldTradeDate = "TRADE_DATE"
	FieldActivDate = "ACTIV_DATE"
	FieldMsgType   = "MSGTYPE"
)

// DefaultFields ETA MARKET_PRICE 要读的字段（与 Java MsgHandler.change / mama.DefaultFields
// 同一集合）。
//
// ⚠️ 不含 MSGTYPE：那是 bridge 的 NATS 契约里由 Java 侧合成的字段（RFC 字典里没有它的 FID），
// ETA 自己从报文 msgClass 判定 UPDATE/REFRESH（Java `MsgHandler` 同样按 msgClass 分支），
// 不需要也不允许把它塞进 Fields（否则字典校验会 fail-closed）。
var DefaultFields = []string{
	FieldBid, FieldAsk, FieldTrdPrc1, FieldMidPrice,
	FieldExchTim, FieldTimAct, FieldTrdTim1, FieldSalTim, FieldQuotim,
	FieldQuotimMs, FieldQuoteDate, FieldTradeDate, FieldActivDate,
	FieldDsplyName,
}
