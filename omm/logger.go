package omm

import (
	"fmt"
	"log"
	"strings"
)

// Logger 是 SDK 使用的**最小日志面**：三个 key-value 风格的方法。
//
// 任何具备同样方法集的日志器都可直接注入（例如 zap/slog 适配器），
// 因此本 SDK 不引入任何日志框架依赖。Config.Logger 为 nil 时使用
// NewStdLogger()（标准库 log，写 stderr）。
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// stdLogger 是标准库 log 的适配器。
type stdLogger struct{}

// NewStdLogger 返回标准库 log 支撑的默认日志器（写 stderr）。
func NewStdLogger() Logger { return stdLogger{} }

func (stdLogger) Info(msg string, kv ...any)  { log.Print("INFO " + msg + kvSuffix(kv)) }
func (stdLogger) Warn(msg string, kv ...any)  { log.Print("WARN " + msg + kvSuffix(kv)) }
func (stdLogger) Error(msg string, kv ...any) { log.Print("ERROR " + msg + kvSuffix(kv)) }

// kvSuffix 把 key-value 对渲染成 "k=v k=v" 后缀（奇数个时只渲染 key）。
func kvSuffix(kv []any) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(kv); i += 2 {
		b.WriteString(" ")
		b.WriteString(fmt.Sprint(kv[i]))
		b.WriteString("=")
		if i+1 < len(kv) {
			b.WriteString(fmt.Sprint(kv[i+1]))
		}
	}
	return b.String()
}
