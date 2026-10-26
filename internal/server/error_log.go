package server

import (
	"context"
	"log/slog"
	"reflect"

	"orbitbeacon/internal/server/storage"
)

// Driver/error messages can contain SQL, bound values, paths and attacker-owned
// text. Log only diagnostic metadata; never pass the error itself to slog.
func (h *handler) logInternalError(err error, requestID string) {
	if code, ok := storage.SQLiteCode(err); ok {
		h.logger.LogAttrs(context.Background(), slog.LevelError, "request_failed",
			slog.String("request_id", requestID), slog.String("error_kind", "sqlite"), slog.Int("sqlite_code", code))
		return
	}
	h.logger.LogAttrs(context.Background(), slog.LevelError, "request_failed",
		slog.String("request_id", requestID), slog.String("error_kind", reflect.TypeOf(err).String()))
}
