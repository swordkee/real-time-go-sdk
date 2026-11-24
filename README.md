# real-time-go-sdk

LSEG **RTSDK（ETA/EMA）** 的**纯 Go** 重实现（协议子集）：RWF 编解码 → RIPC 传输 → EMA 风格的高层
Consumer。**零 cgo、零 `.so`、零 purego** —— `CGO_ENABLED=0` 可编译，Windows/Linux 都能跑，产物是单静态二进制。

> 这是对 LSEG RTSDK C 实现的独立 Go 转译（逐条对照 `Impl/Codec`、`Impl/Transport` 的协议布局），
> 供需要在无闭源库环境下直连 TREP/ADS 的 Go 服务使用。不是 LSEG 官方 SDK，不包含任何 LSEG 二进制或头文件。

**原仓地址（转译来源）**：`https://github.com/Refinitiv/Real-Time-SDK`（LSEG/Refinitiv Real-Time SDK，
Apache-2.0）；本项目对照的是 `7d219a600`（2026-09-16，RTSDK 2.4.1.L1 / ETA C 3.10.1.0）
的 `Cpp-C/Eta/{Impl,Include,etc}` 与 `Cpp-C/Ema/Src`。版权归 LSEG/Refinitiv，见原仓 LICENSE；
本仓只做语言级转换与等价测试，不含原仓任何二进制/头文件。

## 包结构

| 包 | 职责 |
|---|---|
| `github.com/swordkee/real-time-go-sdk/rwf` | RWF（Reuters Wire Format）/ OMM 消息解码：消息头、base key、field list、基础类型、RDMFieldDictionary 解析 |
| `github.com/swordkee/real-time-go-sdk/rwf/encode` | RWF 请求编码：raw login / MARKET_PRICE item request / CLOSE（逐字节对齐 C 侧 `rsslEncodeMsg`） |
| `github.com/swordkee/real-time-go-sdk/ripc` | RIPC over TCP 传输：conn request/connack 握手、v14 key exchange、分片/打包、ping |
| `github.com/swordkee/real-time-go-sdk/native` | 传输客户端外壳：`Connect/Login/Submit/Read/Ping/Shutdown`（RIPC + RWF 编码编排） |
| `github.com/swordkee/real-time-go-sdk/omm` | EMA 高级语义：`Consumer` / `RegisterClient` / 三类回调 / 登录→订阅→重连状态机；`Transport` 接口可注入 |
| `github.com/swordkee/real-time-go-sdk/fields` | MARKET_PRICE 字段名字面值（`BID`/`ASK`…）与默认字段集 |
| `github.com/swordkee/real-time-go-sdk/directory` | RDM Source Directory（domain=4）：请求编码 + serviceId/serviceName 解析（含 DELETE），对照 `rsslRDMDirectoryMsg.c` |
| `github.com/swordkee/real-time-go-sdk/rmtes` | RMTES→UTF-8 全表转换（21,203 表项，机械转写 `rsslRmtes.c`/`rsslcnvtab.c`，Apache-2.0 署名） |
| `examples/eta-probe` | 最小联调示例（环境变量传参，登录 → 订阅 → 收报价） |

`omm.submitPending` 将所有待订阅项打成**一个 RIPC packing 帧**批量提交（对照 C `ipcWriteSession` 的 `IPC_PACKING`），重连后整批重订阅。

依赖图（无环）：`omm → native + rwf + fields`，`native → ripc + rwf/encode`。

## 快速开始

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/swordkee/real-time-go-sdk/omm"
)

func main() {
	cons, err := omm.NewConsumer(omm.Config{
		Host:           "<host>",
		Port:           15000, // 示例端口，按实际 TREP/ADS 配置
		Username:       "your-dacs-user",
		DictionaryFile: "/path/to/RDMFieldDictionary", // FID → 字段名必需
		ServiceIDs:     map[string]uint16{"EXAMPLE": 1},
		Fields:         []string{"BID", "ASK", "TIMACT"},
	})
	if err != nil {
		panic(err)
	}

	client := omm.ClientFunc{
		Refresh: func(m omm.Msg) { fmt.Println("refresh", m.Name, m.Fields) },
		Update:  func(m omm.Msg) { fmt.Println("update", m.Name, m.Fields["BID"], m.Fields["ASK"]) },
		Status:  func(m omm.Msg) { fmt.Println("status", m.Name, m.State) },
	}
	if _, err := cons.RegisterClient(omm.NewReqMsg().Service("EXAMPLE").Item("<ric>"), client); err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = cons.Run(ctx) // 阻塞；内部自动重连并按 watchlist 语义重订阅
}
```

运行示例（参数全部走环境变量，源码不含任何环境信息）：

```bash
CGO_ENABLED=0 go run ./examples/eta-probe
# ETA_HOST / ETA_PORT / ETA_USER / ETA_DICT 必填；
# ETA_SERVICE / ETA_SERVICE_ID / ETA_SYMBOLS（RIC 列表）/ ETA_SECONDS（默认 10）可选
```

## 测试与黄金门禁

全部测试**离线自足**，不依赖 `.so`、外网或本机服务：

- `rwf`：真实 ADS 报文夹具（`rwf/testdata/rwf_frames.txt`，脱敏后的 login refresh + 行情 refresh/update）+ 多组变异锁
  （篡改长度/类型/flags 必须报错，防「宽容解码」回归）。
- `rwf/encode`：**逐字节对拍 C oracle** 的黄金 hex（login、serviceId 单/三字节边界、CLOSE）。
  黄金值由 ETA C 3.10.1 的 `rsslEncodeMsg` 生成，oracle 源码见 `docs/reference/eta-rwf-enc-oracle.c`。
- `ripc` / `native`：本地 TCP 假服务端扮演 RIPC 对端，验证握手/分帧/ping/分片；`-tags eta_live` 提供可选的真机联调测试。
- `omm`：用真实报文驱动 `dispatch`，验证 refresh/update/status → 回调映射与 serviceId 解析 fail-closed。

```bash
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -count=1 ./...

# 零 .so / 零 purego 自证
go list -deps ./... | grep -c 'ebitengine/purego'   # → 0
grep -rn 'dlopen\|librssl' --include='*.go' .        # → 无输出
```

## 范围与限制

- 已实现：raw login（用户名，不带密码）、MARKET_PRICE 域订阅（任意 RIC）、refresh/update/status、
  分片与打包帧、v14 key exchange、心跳、watchlist 式断线重订阅。
- 未实现：directory service 自动发现（serviceId 由配置/`ReqMsg` 显式给出）、压缩协商（不协商，对端回压缩即 fail-closed）、
  除 MARKET_PRICE 外的域、DACS 密码登录。
- 依赖 `RDMFieldDictionary`（LSEG 官方字段字典）做 FID → 字段名映射；仓库内的
  `rwf/testdata/RDMFieldDictionary.fixture` 只是 8 条裁剪夹具，仅供测试。
  完整字典可放到 `RDM_FIELD_DICTIONARY` 环境变量指向的路径，`rwf` 包会跑完整字典断言。

## 设计文档

- [docs/eta-pure-go-poc.md](docs/eta-pure-go-poc.md) —— 纯 Go 直译可行性验证（RWF 解码逐字节对照、真实报文）
- [docs/eta-omm-design.md](docs/eta-omm-design.md) —— EMA 高级层（omm）设计与传输注入
- [docs/reference/eta-rwf-enc-oracle.c](docs/reference/eta-rwf-enc-oracle.c) —— 生成黄金编码 hex 的 C oracle

## License

MIT，见 [LICENSE](LICENSE)。
