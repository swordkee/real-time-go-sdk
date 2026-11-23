# ETA RWF/OMM 纯 Go 直译可行性评估（PoC）

> **结论：整条链路已纯 Go 打通**。解码（`rwf`）、请求编码（`rwf/encode`）、RIPC 传输（`ripc`）
> 与客户端外壳（`native`）全部为纯 Go，`CGO_ENABLED=0` 编译出的 `examples/eta-probe`
> 不含任何 cgo/dlopen 符号（`ldd` = not a dynamic executable）。
>
> 本文 §1-§3 是解码侧 PoC 的记录与门禁；§4 是工作量表；**§8 是传输层/编码侧的语言级转换说明**。

## 1. 参照源（RTSDK C 源码）

| 用途 | 位置 |
| --- | --- |
| 线上格式权威实现（逐条翻译依据） | `Real-Time-SDK/Cpp-C/Eta/Impl/Util/Include/rtr/rwfNetwork.h`（`rwfGet*` 原语）、`Impl/Codec/messageDecoders.c`（消息头）、`Impl/Codec/fieldListDecoder.c`、`Impl/Codec/primitiveDecoders.c`、`Impl/Codec/rwfConvert.c`（Real→字符串） |
| 枚举/结构定义 | `Real-Time-SDK/Cpp-C/Eta/Include/Codec/rtr/*.h` |
| 域模型规格 | `Docs/ETAC_DevGuide.pdf`、`ETAC_RDMUsageGuide.pdf`、`API_ConceptsGuide.pdf` |
| 字段字典 | `RDMFieldDictionary`、`enumtype.def` |
| 报文夹具 | `rwf/testdata/rwf_frames.txt`（4 帧，已脱敏，见 §3） |

## 2. 最小能力集（PoC 已实现的 6 项）

1. **变长原语**（最容易翻错，全部逐条对照 `rwfNetwork.h`）
   - `rwfGetResBitU15`（1 或 2 字节，2 字节时 `&0x7FFF`）
   - `rwfGetResBitI15`（1 字节时的 `0x40` 负号窗口 vs 2 字节的保留位掩码）
   - `rwfGetOptByteU16`（`<0xFE` 单字节 / `0xFE`+2 字节）
   - `rwfGetBuffer8` / `rwfGetBuffer16`（缓冲长度前缀，`rwfGetBuffer16` 是 `0xFE` 双形态）
   - 「Size 变体」`rwfGetLenSpecU64Size/I64Size`（入参=**剩余字节数**，与 lenSpec 变体不同）
2. **消息头**（`rsslDecodeMsgHeader`）：2 字节 headerSize + msgClass(掩 `0x1F`) + domainType +
   streamId，然后按 class 分派；`encDataBody` **没有长度前缀**，直接到报文末尾。
   已实现 Refresh / Update / Status / Request / Generic 五个分支。
3. **MsgKey**（`rsslDecodeBaseKey`）：flags→serviceId/name/nameType/filter/identifier/attrib 条件字段。
4. **State / Qos**（`_rsslDecodeState` / `_rsslDecodeQos`）。
5. **FIELD_LIST**（`rsslDecodeFieldList`/`rsslDecodeFieldEntry`）：flags、HasInfo(dictId/listNum)、
   HasSetData/HasSetId、标准条目 `uint16 fid + buffer16 payload`。
6. **字段值解码 + 字典**：REAL（`rwfGetReal64` + hint 的指数/分数两套语义）、UInt/Int/Enum、
   Date/Time/DateTime、Buffer/RMTES/ASCII；`RDMFieldDictionary` 解析（FID↔名↔RWF 类型）。

## 3. PoC 验证证据

### 3.1 报文夹具（已脱敏）

夹具为 4 帧真实 TREP/ADS 报文（已去掉 RIPC 传输头），其中 **login 用户名与主机名已替换为同长度占位**
（`username` / `server`），字节布局不变：

| 帧 | 大小 | 内容 |
| --- | --- | --- |
| 0 | 385B | login REFRESH，state Open/Ok，text `Login accepted by host server.` |
| 1 | 1334B | `<ric>` REFRESH，239 条 field，含 key + serviceId |
| 2 | 190B | `<ric>` UPDATE，19 条 field，seq=50222 |
| 3 | 31B | `<ric>` UPDATE，3 条 field（只有 BID_1 族），seq=50238 |

### 3.2 与 C 库逐字节对拍（最强锁）

C 侧 oracle 打印每个 field entry 的 **payload hex**，Go 侧测试断言同一份 hex：

```
BID        fid=22   payload=0a068268  → 42.6600
ASK        fid=25   payload=0a06845c  → 42.7100
MID_PRICE  fid=134  payload=0c10ad    → 42.69
TIMACT     fid=5    payload=062d      → 06:45:00
ACTIV_DATE fid=17   payload=170b07ea  → 2026-11-23
QUOTIM_MS  fid=3855 payload=0173926b  → 24351339
DSPLY_NAME fid=3    payload=42524f4b455220202020202020474658 → "BROKER       GFX"（去空格 → BROKERGFX）
```

### 3.3 门禁与变异自检

```
$ go build ./...                                  rc=0
$ go vet ./rwf/                                   rc=0
$ go test -count=1 ./rwf/
  TestOfficialDictionary   PASS  （官方字典解析 + 真实报文复解）
  TestDecodeRealFrames     PASS  （4 帧全链：头/key/state/fieldlist/payload hex/报价字段）
  TestRealString           PASS  （REAL→字符串 13 个用例：标度/负数/分数/Inf/NaN/blank）
  TestDecodeRealWire       PASS  （线上 REAL 编码，负值在定点值符号位上）
  TestWirePrimitives       PASS  （ResBitU15/OptByteU16/buffer8/buffer16/Size 变体）
  TestDecodeTimeDate       PASS
  TestTruncatedFrame       PASS  （截断整链必须报错，不 panic）
  ExampleDecodeMsg         PASS
```

**变异自检 8/8 被抓**（把实现改坏 → 测试必须红）：

| 变异 | 结果 |
| --- | --- |
| buffer16 长形态长度读错 | RED |
| resBitU15 不清保留位 | RED |
| REAL 指数方向翻转 | RED |
| field entry fid 读成 1 字节 | RED |
| 字典 REAL64 类型映射丢失 | RED |
| msgKey serviceId 不读 | RED |
| refresh 不解 state | RED |
| UInt 按符号扩展 | RED（首次为假绿，补 `DecodeUInt(0xFFFF)=65535` 锁后转红） |

## 4. 工作量评估（完整替换 ETA C 读数链）

| 工作项 | C 参照体量 | 纯 Go 估量 | 人天 | 风险 |
| --- | --- | --- | --- | --- |
| RWF 原语 + 消息头 + FIELD_LIST + 字典（**已完成**） | `rwfNetwork.h` + 3 个 decoder ≈2500 行 | ≈1030 行 | 2（已花） | 低：已对拍 |
| 其他容器：ELEMENT_LIST / MAP / SERIES / VECTOR / FILTER_LIST / ARRAY | ≈1300 行 | ≈900 行 | 3 | 低-中：目录/登录响应要用 |
| REAL/DATE/TIME/DATETIME 全量边界（含 blank 哨兵、微秒/纳秒） | `numeric.c`+`dtime.c` ≈1900 行 | ≈600 行 | 2 | 低 |
| RMTES → UTF-8（DSPLY_NAME 的中文/扩展字符） | `rsslRmtes.c` 1339 行 | ≈500 行 | 2-3 | 中：字符集表要从 `rsslcnvtab.c` 抽；**未实现** |
| 消息**编码**（Request/Login/字段列表构造）（**已完成**，见 §8.2） | `messageEncoders.c`+`primitiveEncoders.c` ≈3700 行 | ≈1200 行 | 4 | 中：必须与 C 逐字节一致才能被 ADS 接受 |
| RIPC 传输层（握手/connack/心跳/fragment）（**已完成**，见 §8.1） | `rsslSocketTransportImpl.c` 1.1 万行（大多 SSL/HTTP/WS，可裁） | ≈1500 行 | 5-7 | 中-高：握手版本协商/connack 错会「连上但立刻断」；**压缩未协商（位图 0）故未实现 LZ4/zlib** |
| DACS 登录 + 目录请求 + 服务 id 解析 | 域模型 + `msgs.c` | ≈500 行 | 2 | 中：目录请求要发 ElementList，且要处理 multipart 目录 |
| 状态机/重连/心跳/订阅编排 | — | ≈800 行 | 3-4 | 中 |
| 测试夹具与对拍工具（C oracle + 逐字节比对） | — | ≈400 行 | 2 | 低 |

**合计 ≈25-30 人天**（1 人全职、含测试）。**已落地**：解码 + 编码 + RIPC 传输 + native 外壳 + 门禁测试
⇒「能连上去并订阅」这半边已完成；剩余主要是目录自动发现、RMTES、压缩与生产接线。

风险排序：
1. **RIPC 传输层**（已实现，风险降为「压缩/分片边界」）：握手失败的表现很隐晦——本实现用
   单元测试锁死 conn request/connack/key exchange 布局，并可用 `examples/eta-probe` 在自有环境联调。
2. **编码必须逐字节对齐**（已解）：C oracle 生成黄金 hex 做门禁（§8.2）。
3. **RMTES 字符集**（未做）：对端若下发中文/日文就需要转换表。
4. 压缩（LZ4）与 fragmentation：**压缩主动不协商**（位图长度 0），fragmentation 已实现装配。

## 5. 与「动态加载 C 库」路线对比

| 维度 | 纯 Go 直译（本文） | 动态加载 C 库（dlopen/purego） |
| --- | --- | --- |
| 部署依赖 | **零**（单静态二进制，Windows 也能跑） | 需 `librssl.so`+`librsslVA.so` + `LD_LIBRARY_PATH`，且通常只能 linux/darwin |
| 架构绑定 | 无 | `RsslMsg`/`RsslDecodeIterator` 等结构体布局硬编码，**SDK 升级 = 可能踩内存** |
| 可见性/可调试 | 全在 Go 源码里，可单测、可 fuzz | 黑盒；只能靠日志猜 C 内部状态 |
| 维护成本 | 上游升级需跟（RWF 线上格式 20 年未变，风险低） | 每次 SDK 升级都可能破 ABI |
| 合规/授权 | 需遵守 RTSDK 许可（Apache 2.0，可参考实现） | 直接分发闭源 `.so` |

若目标是**彻底摆脱 .so**（Windows/多架构可跑、镜像里不分发闭源库），纯 Go 是唯一解。

## 6. 本仓库交付物

- `rwf/`（纯 Go，无 build tag，Windows/WSL 均可 build+test）
  - `wire.go` 变长原语 · `message.go` 消息头/key/state/qos · `fieldlist.go` 字段表
  - `value.go` 字段值（REAL/UInt/Int/Enum/Date/Time/DateTime/Buffer）
  - `dict.go` RDMFieldDictionary 解析 · `quote.go` 报价字段提取
  - `testdata/` 脱敏报文 4 帧 + 裁剪字典夹具 · `rwf_test.go` / `example_test.go`
- `rwf/encode/`、`ripc/`、`native/`、`omm/`、`fields/`（见 `README.md`）
- `examples/eta-probe/`（纯 Go 联调示例，参数全部走环境变量）

## 7. 复现方式

```bash
export GOWORK=off GOFLAGS=-mod=mod CGO_ENABLED=0
go test -count=1 -v ./rwf/
go test -count=1 ./...
```

C oracle（可选，需自备 RTSDK 头文件与 `librssl.so`）：`docs/reference/eta-rwf-enc-oracle.c`
用于生成/复核黄金 hex；仓库内测试使用其输出常量，不依赖 `.so` 运行时。

## 8. 传输层 + 编码侧纯 Go 化

### 8.1 包与对应 C 源码

| 包 | 职责 | 逐条对照的 C |
| --- | --- | --- |
| `rwf/encode` | RWF 请求编码（login / MARKET_PRICE item / close） | `Impl/Codec/messageEncoders.c`（rsslEncodeMsg→rsslEncodeMsgInternal→rsslEncodeKeyInternal）、`rwfNetwork.h` 的 rwfPut* |
| `ripc` | TCP + RIPC：conn request / connack / v14 key exchange / 数据帧（extended flags、fragmentation、packing）/ ping | `Impl/Transport/rsslSocketTransportImpl.c`（ipcConnecting / ipcWaitAck / ipcSendClientKey / ipcReadSession / ipcIntWrtHeader）、`ripc_int.h` |
| `native` | 客户端外壳，方法集对齐 raw 客户端 | — |
| `fields` | 字段名唯一来源（让 omm 不依赖传输实现） | RDMFieldDictionary |

### 8.2 编码侧：与 C 逐字节对拍（最硬的门禁）

用 RTSDK C（3.10.1）的 `rsslEncodeMsg` 现场编码出黄金 hex（oracle 源码
`docs/reference/eta-rwf-enc-oracle.c`），Go 侧测试断言完全相等。基准使用占位参数
（`user=username` / `serviceId=999` / `name=TEST`），布局与真实请求一致：

| 消息 | 字节数 | 黄金 hex |
| --- | --- | --- |
| login（user=`username`, stream=1） | 23 | `00150101000000010400800b0608757365726e616d6501` |
| item（`TEST`, serviceId=999, stream=3） | 22 | `00140106000000030400800a07fe03e7045445535401` |
| item（serviceId=100，单字节形态） | 20 | `0012010600000003040080080764045445535401` |
| item（serviceId=254，三字节边界） | 22 | `00140106000000030400800a07fe00fe045445535401` |
| close（stream=3） | 10 | `00080506000000030000` |

要点（容易写错的三个细节）：
1. **key 长度恒写 2 字节 RB15**（`rwfPut16(lenPos, keySize|0x8000)`），即使 <0x80；
2. `serviceId` 用 `optByteU16`：`<0xFE` 单字节、否则 `0xFE`+2 字节（254 也走三字节）；
3. `containerType` 线上值要减 `RSSL_DT_CONTAINER_TYPE_MIN(128)`（NO_DATA → 0）。

### 8.3 传输侧：握手与收帧

- **conn request**：`<u16 len><u8 opCode=0><u32 connVersion=0x17><u8 flags=RIPC_KEY_EXCHANGE>`
  `<u8 hdrSize><u8 压缩位图长度=0><u8 ping秒><u8 rsslFlags=0><u8 protocolType=0>`
  `<u8 rwf major=14><u8 minor=1><u8 hostLen><host><u8 addrLen><addr><u8 组件版本总长=1><u8 串长=0>`；
  单测锁死 32 字节黄金布局（host/addr 用示例值）。
- **connack**：解析 NAK 文本、`ripcVersion`、`maxUserMsg`、`rsslFlags/ping/major/minor/comp`；
  v14 再解析 `KEY_EXCHANGE 标记 + 类型 + 长度 + P/G/send_key`（偏移与 C `ipcWaitAck` 一致：
  19/20/21/22..30/30..38/38..46）。
- **key exchange 第三腿**：`r ∈ [2,p-2)`，`client_send_key = G^r mod P`，回
  `<u16 12><u8 0x08><u8 8><8B key>`；对端类型未知时回 0 长度（与 C 一致，fail-safe）。
- **压缩**：conn request 压缩位图长度 **0** ⇒ 服务端不压缩；若 connack 仍指定压缩类型，
  或收到 `IPC_COMP_DATA` 帧，**fail-closed 报错**（绝不静默解错字节）。LZ4/zlib 未实现。
- **fragmentation**：`IPC_FRAG_HEADER`（总长 4B + fragId 2B，v13+）+ `IPC_FRAG` 装配，
  总长上限 64MB 防 OOM；**packing**：`[u16 len][msg]…` 拆成多条；**ping**：3 字节空数据帧
  （`00 03 02`），收到回一次。

### 8.4 门禁与自证

```
CGO_ENABLED=0 go build ./...    rc=0
CGO_ENABLED=0 go vet ./...      rc=0
CGO_ENABLED=0 go test ./...     rc=0
纯 Go 二进制自证（/tmp/eta-probe）:
  go list -deps | grep ebitengine/purego  → 无
  grep -c dlopen                          → 0
  ldd                                     → not a dynamic executable
```

实网联调用 `examples/eta-probe`（参数全部走环境变量，仓库源码不含任何环境信息）：

```
ETA_HOST=<host> ETA_PORT=<port> ETA_USER=<user> ETA_DICT=<dict> \
ETA_SERVICE=<service> ETA_SERVICE_ID=<id> ETA_SYMBOLS=<ric> CGO_ENABLED=0 go run ./examples/eta-probe
```

### 8.5 顺带修正的缺陷

`rwf` 的 `RequestMsgFlags` 常量与 RTSDK 头文件不符（旧值 `Streaming=0x1`/`Priority=0x4`…），
回解 REQUEST 时会把 `STREAMING(0x4)` 当作 `HAS_PRIORITY` 多吃 2 字节、key 全解错。
已在 `rwf/message.go` 按 `rsslRequestMsg.h` 订正并注明触发条件。

### 8.6 仍未做（剩余项）

- **RMTES→UTF-8**（DSPLY_NAME 中文/扩展字符）：未实现。
- **LZ4/zlib 压缩**：主动不协商；若对端强开压缩才需要。
- **directory 自动发现 serviceName→serviceId**：当前仍是配置驱动。
- 消息 **ELEMENT_LIST/MAP/SERIES/VECTOR** 容器解码（directory 响应要用）。
