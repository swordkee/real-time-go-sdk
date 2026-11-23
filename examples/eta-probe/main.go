package main

// eta-probe：用 **纯 Go** EMA（omm.Consumer + native 传输 + rwf 解码 + rwf/encode 编码）
// 联调 TREP/ADS：login → 订阅 → 收 BID/ASK。
//
// 本示例**零 .so**：CGO_ENABLED=0 可编译，Windows/Linux 都能跑。
// 参数全部走环境变量（源码不硬编码任何环境信息）：
//
//	ETA_HOST        必填             TREP/ADS 地址（如 <host>）
//	ETA_PORT        必填             端口（如 <port>）
//	ETA_USER        必填             DACS 用户名
//	ETA_DICT        必填             RDMFieldDictionary 路径
//	ETA_SERVICE     可选             service 名（与 ETA_SERVICE_ID 一起用）
//	ETA_SERVICE_ID  可选             数字 serviceId
//	ETA_SYMBOLS      可选             逗号分隔 RIC（占位默认 TEST，按需指定）
//	ETA_SECONDS     可选             运行秒数，默认 10
//
import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/swordkee/real-time-go-sdk/omm"
)

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	host := os.Getenv("ETA_HOST")
	port := envInt("ETA_PORT", 0)
	user := os.Getenv("ETA_USER")
	dict := os.Getenv("ETA_DICT")
	if host == "" || port <= 0 || user == "" || dict == "" {
		fmt.Fprintln(os.Stderr, "用法: ETA_HOST=.. ETA_PORT=.. ETA_USER=.. ETA_DICT=.. [ETA_SERVICE=..] [ETA_SERVICE_ID=..] [ETA_SYMBOLS=TEST] [ETA_SECONDS=10] go run ./examples/eta-probe")
		os.Exit(2)
	}

	service := envStr("ETA_SERVICE", "")
	serviceID := uint16(envInt("ETA_SERVICE_ID", 0))
	symbols := strings.Split(envStr("ETA_SYMBOLS", "TEST"), ",")
	seconds := envInt("ETA_SECONDS", 10)

	cfg := omm.Config{
		Host:           host,
		Port:           port,
		Username:       user,
		DictionaryFile: dict,
		Fields:         []string{"BID", "ASK", "TIMACT", "DSPLY_NAME"},
		PingInterval:   10 * time.Second,
	}
	if service != "" && serviceID > 0 {
		cfg.ServiceIDs = map[string]uint16{service: serviceID}
	}
	if serviceID > 0 {
		cfg.DefaultServiceID = serviceID
	}
	// 本示例刻意**不 import 任何闭源库代码**（纯 Go 证据链）。

	cons, err := omm.NewConsumer(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "NewConsumer err:", err)
		os.Exit(2)
	}

	var refresh, update, status int
	client := omm.ClientFunc{
		Refresh: func(m omm.Msg) {
			refresh++
			if refresh <= 3 {
				fmt.Printf("REFRESH item=%s BID=%s ASK=%s TIMACT=%s fields=%d\n",
					m.Name, m.Fields["BID"], m.Fields["ASK"], m.Fields["TIMACT"], len(m.Fields))
			}
		},
		Update: func(m omm.Msg) {
			update++
			if update <= 5 {
				fmt.Printf("UPDATE[%d] item=%s BID=%s ASK=%s TIMACT=%s DSPLY_NAME=%q\n",
					update, m.Name, m.Fields["BID"], m.Fields["ASK"], m.Fields["TIMACT"], m.Fields["DSPLY_NAME"])
			}
		},
		Status: func(m omm.Msg) {
			status++
			fmt.Printf("STATUS item=%s state=%+v\n", m.Name, m.State)
		},
	}

	for _, sym := range symbols {
		sym = strings.TrimSpace(sym)
		if sym == "" {
			continue
		}
		if _, err := cons.RegisterClient(omm.NewReqMsg().Service(service).Item(sym), client); err != nil {
			fmt.Fprintln(os.Stderr, "RegisterClient err:", err)
			os.Exit(2)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	if err := cons.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Run err:", err)
	}
	fmt.Printf("=== eta-probe done: refresh=%d update=%d status=%d ===\n", refresh, update, status)
	if refresh+update == 0 {
		os.Exit(1)
	}
}
