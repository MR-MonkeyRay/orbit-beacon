package app

import (
	"context"
	"database/sql"
	"log/slog"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/history"
)

// 历史记录异常不得阻断监控；恢复主库后遗留的 metrics.db 可能使用更新的 schema。
func openMetricHistory(ctx context.Context, dataDir string, nodes *sql.DB, logger *slog.Logger) *history.Service {
	service, err := history.Open(ctx, dataDir, nodes, history.Options{Logger: logger})
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "history_disabled", logging.ErrorAttrs(err, "history_open_failed")...)
		return nil
	}
	return service
}
