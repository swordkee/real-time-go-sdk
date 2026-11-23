package omm

import "log/slog"

// logger 取日志器：Config.Logger 为 nil 时用 slog.Default()。
// SDK 只依赖标准库 log/slog，不自定义日志接口（业务方可直接传 *slog.Logger）。
func (c *Consumer) logger() *slog.Logger {
	if c.cfg.Logger == nil {
		return slog.Default()
	}
	return c.cfg.Logger
}
