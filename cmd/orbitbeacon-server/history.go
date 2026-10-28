package main

import (
	"context"
	"database/sql"
	"log/slog"

	"orbitbeacon/internal/server/history"
)

// History is best-effort and excluded from backups. In particular, restoring a
// main database alongside a newer metrics.db must not stop monitoring service.
func openMetricHistory(ctx context.Context, dataDir string, nodes *sql.DB, logger *slog.Logger) *history.Service {
	service, err := history.Open(ctx, dataDir, nodes, history.Options{Logger: logger})
	if err != nil {
		logger.Warn("metric history disabled: cannot open metrics.db; the main database remains available", "error", err)
		return nil
	}
	return service
}
