package logging

import (
	"context"
	"log/slog"
)

// MySQLLogger 丢弃驱动自由文本，只提取真实错误的安全诊断属性。
type MySQLLogger struct{}

func (MySQLLogger) Print(values ...any) {
	for _, value := range values {
		if err, ok := value.(error); ok && err != nil {
			slog.Default().LogAttrs(context.Background(), slog.LevelWarn, "MySQL driver failure", ErrorAttrs(err, "storage_driver")...)
		}
	}
}
