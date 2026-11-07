package app

import (
	"log/slog"
	"time"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/storage"
)

func openMetricHistory(main, metrics *storage.Database, logger *slog.Logger, observeFlush func(time.Duration, int)) *history.Service {
	if metrics == nil {
		return nil
	}
	return history.OpenWithDatabase(metrics, main, history.Options{Logger: logger, ObserveFlush: observeFlush})
}
