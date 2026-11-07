package app

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/history"
)

// 历史记录异常不得阻断监控；恢复主库后遗留的 metrics.db 可能使用更新的 schema。
func openMetricHistory(ctx context.Context, cfg config.Server, nodes *sql.DB, logger *slog.Logger, observeFlush func(time.Duration, int)) *history.Service {
	opts := history.Options{Logger: logger, ObserveFlush: observeFlush}
	var service *history.Service
	var err error
	if cfg.HistoryVictoriaMetricsURL == "" {
		service, err = history.Open(ctx, cfg.DataDir, nodes, opts)
	} else {
		service, err = history.OpenVictoriaMetrics(ctx, cfg.DataDir, cfg.HistoryVictoriaMetricsURL, cfg.HistoryVictoriaMetricsToken, nodes, opts)
	}
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "history_disabled", logging.ErrorAttrs(err, "history_open_failed")...)
		return nil
	}
	return service
}
