package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"reflect"

	"orbitbeacon/internal/server/auth"

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

// Failed and throttled logins are logged for brute-force investigation. The
// submitted username and password are never logged: users sometimes type a
// password into the username field.
func (h *handler) logLoginRejection(err error, r *http.Request, requestID string) {
	reason := ""
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		reason = "invalid_credentials"
	case errors.Is(err, auth.ErrRateLimited):
		reason = "rate_limited"
	default:
		return
	}
	peer, _, splitErr := net.SplitHostPort(h.clientAddr(r))
	if splitErr != nil {
		peer = h.clientAddr(r)
	}
	h.logger.LogAttrs(context.Background(), slog.LevelWarn, "login_rejected",
		slog.String("request_id", requestID), slog.String("peer", peer), slog.String("reason", reason))
}
