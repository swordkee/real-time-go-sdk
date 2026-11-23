# EMA 高级层：纯 Go 直译 OmmConsumer（设计）

> 本仓库用**纯 Go** 实现了一套等价于 EMA `OmmConsumer` 的高级 API：`Consumer` + `RegisterClient`
> + `OnRefreshMsg/OnUpdateMsg/OnStatusMsg` + 自动 login/订阅/重连（watchlist 语义）。
> 应用侧只写「订阅 service / item → 回调拿字段」，不碰字节级协议。
> 全链路零 cgo / 零 `.so`：`CGO_ENABLED=0` 可编译，Windows 也能跑。

本文是 `docs/eta-pure-go-poc.md`（RWF/OMM 解码 PoC）的**上层补充**：那份证明「解码能纯 Go」，
本文说明「EMA 高级语义如何用纯 Go 表达」，以及每一层与 RTSDK C 源码的对应关系。

## 1. 分层

```
应用层    订阅 service / item，回调拿字段
omm       Consumer / RegisterClient / 回调 / login-订阅-重连状态机
rwf       RWF/OMM 解码（消息头 / FieldList / 值 / 字典）
rwf/encode RWF 请求编码（login / item / close，逐字节对齐 C）
ripc      RIPC 传输（TCP 握手 / connack / key exchange / 心跳 / 收帧）
native    客户端外壳（Connect/Login/Submit/Read/Ping/Shutdown）
```

- **传输层**：`ripc` 逐条对照 `rsslSocketTransportImpl.c`
  （conn request / connack / v14 key exchange / 数据帧 / extended flags / fragmentation / packing / ping）。
- **解码/编码层**：`rwf` + `rwf/encode` 都是纯 Go；编码有 C oracle 的黄金 hex 对拍门禁。
- **传输可注入**：`omm.Transport` 是接口，`Config.TransportFactory` 可注入自定义实现（例如业务方
  用闭源库做对照）；本仓库默认走纯 Go `native`，不 import 任何闭源库。

## 2. EMA API ↔ Go 映射

| EMA (Java/C++) | 本实现 (Go) | 说明 |
| --- | --- | --- |
| `OmmConsumerConfig().host(h).username(u)` | `omm.Config{Host,Port,Username,...}` | 字典/字段/服务映射也在 Config |
| `EmaFactory.createOmmConsumer(cfg)` | `omm.NewConsumer(cfg)` | 加载字典 + 构造传输，**不连接** |
| `consumer.registerClient(reqMsg, client)` | `consumer.RegisterClient(req, client)` | 返回 `uint64` 句柄；可 Run 前/后调用 |
| `ReqMsg().serviceName(s).name(ric)` | `omm.NewReqMsg().Service(s).Item(ric)` | serviceName→serviceId 见 §4 |
| `OmmConsumerClient.onRefreshMsg(msg,event)` | `Client.OnRefreshMsg(omm.Msg)` | |
| `onUpdateMsg` / `onStatusMsg` | `OnUpdateMsg` / `OnStatusMsg` | 另有 `ClientFunc` 适配器只填需要的回调 |
| `consumer.dispatch()` / 事件循环 | `consumer.Run(ctx)` | 阻塞；ctx 取消即退出 |
| watchlist 自动 login/目录/重连重订阅 | `Run` 内状态机 | 见 §3 |

消息模型 `omm.Msg`：`Class/Domain/StreamID/ServiceID/Name/State/Fields/At`。
`Fields` 是 `map[string]string`（价格取 REAL→字符串原样、时间 `HH:MM:SS`、日期 `yyyy-mm-dd`、
`DSPLY_NAME` 去空格）。

## 3. 状态机（Run 内）

```
Connect(RIPC 握手 → ACTIVE)
  → Login(raw REQUEST/LOGIN, msgKey.name=username)
  → 收 login REFRESH:
       StreamState=OPEN && DataState=OK  ⇒ loggedIn，提交全部订阅(submitPending)
       否则 ⇒ 报错（fail-closed）
  → 收 login STATUS ⇒ 报错（登录被拒，fail-closed）
  → 读循环：ripc.Read → rwf.DecodeMsg →
        domain=LOGIN → 上面登录判定
        domain=MARKET_PRICE → 按 streamId 找订阅 → 按 msgClass 回调
  读出错/通道断 ⇒ Shutdown + 退避(1s 指数到 30s) 重连 ⇒ 重连后 resetSubmitted 全部重发(重订阅)
```

- **watchlist 语义**：重连后所有订阅自动重新 item request，客户端无需感知。
- **Run 期间新注册**：read 循环 WOULD_BLOCK 时顺带 `submitPending`，把新订阅补发（单 goroutine 写，避免并发写 channel）。
- 心跳：主动 `Ping`（默认 15s）+ 收到对端 PING 回 Ping。

## 4. serviceName → serviceId

EMA 内部用 **directory（domain=DIRECTORY, stream 2）** 请求拿 serviceId。本库先采用**配置驱动**：

```go
Config.ServiceIDs map[string]uint16{"EXAMPLE": 42}
```

解析顺序：显式 `ReqMsg.ServiceID` > `ServiceIDs[name]` > `DefaultServiceID`，都取不到则 **fail-closed 报错**。
**完整 directory 自动发现列为剩余工作**（见 §7）。

## 5. 验证方式（全部离线、可复跑）

- `omm` 测试用脱敏后的真实报文夹具（`rwf/testdata/rwf_frames.txt`）驱动 `dispatch`：
  `TestDispatchUpdateAndRefresh`（refresh+update → 回调拿到字段）、`TestDispatchStatus`（State 映射）、
  `TestResolveServiceID`、`TestRegisterClientAssignsStream`（streamId 从 3 起）、`TestClientFuncNilSafe`。
- `native` / `ripc` 测试用本地 TCP 假服务端扮演 RIPC 对端，验证握手/分帧/心跳/分片，以及 login/item
  帧字节与黄金值一致。
- 需要实网验证时，用 `examples/eta-probe`（参数全部走环境变量，源码不含任何环境信息）：

```bash
CGO_ENABLED=0 go run ./examples/eta-probe
# ETA_HOST=<host> ETA_PORT=<port> ETA_USER=<user> ETA_DICT=<RDMFieldDictionary>
# 可选 ETA_SERVICE=<service> ETA_SERVICE_ID=<id> ETA_SYMBOLS=<ric,ric> ETA_SECONDS=10
```

## 6. 与 cgo / 闭源封装路线对比

| 维度 | 本方案（全纯 Go） | cgo 包 C 库 | 动态加载 C 库（dlopen/purego） |
| --- | --- | --- | --- |
| 业务/格式逻辑 | 纯 Go（可读、可单测、可 fuzz） | 在 C 里 | 黑盒 C API |
| 部署依赖 | **零 .so**（单静态二进制；CGO_ENABLED=0） | 需 .so + cgo 工具链 | 需 .so |
| 架构绑定风险 | 无（无结构体布局依赖，无 ABI） | 高（cgo 全量绑定） | 高（解码结构体 `RsslDecodeIterator=936B` 等硬编码） |
| Windows/多架构 | **Windows 也可 build+run**（纯 Go，无 build tag） | 基本不行 | 不行 |
| 高级语义（watchlist/重连/回调） | **有** | 无（要自己写） | 无（C 侧是扁平 Handler） |
| 工作量 | 解码/编码/传输/外壳全自研 | — | — |

## 7. 剩余工作量（从「能收行情」到「生产级 EMA 等价」）

| 项 | 估量 | 风险 | 说明 |
| --- | --- | --- | --- |
| directory 自动发现 serviceName→serviceId（domain=DIRECTORY 请求 + ElementList/Map 解 service 表） | 2-3 人天 | 中 | 需补 rwf 的 ELEMENT_LIST/MAP 解码；替代当前配置驱动 |
| 字典下载（dictionary domain）或本地字典热加载 | 1-2 人天 | 低 | 当前依赖本地 RDMFieldDictionary 文件 |
| item 级 status/恢复策略细化（StreamClosed/恢复后重订阅、permission 拒绝、item 失效重建） | 2-3 人天 | 中 | 目前统一「断开重连即全量重发」 |
| RMTES→UTF-8（中文/扩展字符 DSPLY_NAME） | 2-3 人天 | 中 | 当前未实现；对端下发中文时必须补 |
| LZ4/zlib 压缩 | 已完成 | 低-中 | **客户端不请求压缩**（conn request 位图长度 0）；服务端强制时按 C 语义解压（zlib 持续流 / LZ4 block，见 `ripc/compress.go`），未知类型 fail-closed |
| 生产接线（把 omm 接到具体业务的下游过滤/分发层） | 1-2 人天 | 低 | omm 交付全部 refresh+update，过滤属适配层 |

## 8. 构建与复现

```bash
export GOWORK=off GOFLAGS=-mod=mod CGO_ENABLED=0
go build ./...
go vet ./...
go test -count=1 ./...

# 传输层联调测试（仅显式开启，未设环境变量时 skip）
RIPC_LIVE_ADDR=<host>:<port> RIPC_LIVE_USER=<user> RIPC_LIVE_SERVICE_ID=<id> \
  go test -tags eta_live -run 'TestLive' -v ./ripc/
```

## 9. 包 ↔ C 源码对照

| 包 | 职责 | 逐条对照的 C |
| --- | --- | --- |
| `rwf/encode` | RWF 请求编码（login / MARKET_PRICE item / close） | `Impl/Codec/messageEncoders.c`（rsslEncodeMsg→rsslEncodeMsgInternal→rsslEncodeKeyInternal）、`rwfNetwork.h` 的 rwfPut* |
| `ripc` | TCP + RIPC：conn request / connack / v14 key exchange / 数据帧 / ping | `Impl/Transport/rsslSocketTransportImpl.c`（ipcConnecting / ipcWaitAck / ipcSendClientKey / ipcReadSession / ipcIntWrtHeader）、`ripc_int.h` |
| `native` | 客户端外壳：Connect/Login/Submit/CloseStream/Read/Ping/Shutdown | 对应 C 侧 raw 客户端调用序 |
| `rwf` | RWF/OMM 解码 + RDMFieldDictionary | `messageDecoders.c` / `fieldListDecoder.c` / `primitiveDecoders.c` / `rwfConvert.c` |
| `fields` | MARKET_PRICE 字段名字面值 | RDMFieldDictionary 字段名 |

**C oracle（黄金 hex 来源，可重放）**：`docs/reference/eta-rwf-enc-oracle.c` 用
`rsslInitialize + rsslClearMsg + rsslSetEncodeIteratorRWFVersion(14,1) + rsslEncodeMsg` 打印 hex。
若换 SDK 版本，重跑 oracle 并更新 `rwf/encode/encode_test.go` 的常量即可。
黄金基准使用占位参数（`user=username` / `serviceId=999` / `name=TEST`），字节布局与真实请求一致。

## 10. 已知修复

`rwf` 的 `RequestMsgFlags` 常量曾与 RTSDK 头文件不符（旧值 `Streaming=0x1`/`Priority=0x4`…），
导致回解 REQUEST 时把 `STREAMING(0x4)` 当作 `HAS_PRIORITY` 多吃 2 字节、key 全解错。
已按 `rsslRequestMsg.h` 逐条订正（`rwf/message.go` 有注释说明触发条件）。

## 11. 零依赖自证

```bash
go list -deps ./... | grep -c 'ebitengine/purego'   # → 0
grep -rn 'dlopen' --include='*.go' .                 # → 无输出
CGO_ENABLED=0 go build ./...                         # → rc=0
```
