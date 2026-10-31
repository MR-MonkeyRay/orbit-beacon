package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/auth"
)

// 驱动/错误消息可能包含 SQL、绑定值、路径和攻击者可控的文本。
// 只记录诊断元数据；绝不把错误本身传给 slog。
func (h *handler) logInternalError(err error, requestID string) {
	attrs := append(logging.ErrorAttrs(err, "internal_error"), slog.String("request_id", requestID))
	h.logger.LogAttrs(context.Background(), slog.LevelError, "request_failed", attrs...)
}

// 失败和被限流的登录会被记录，用于暴力破解调查。
// 提交的用户名和密码绝不记录：用户有时会把密码输到用户名栏。
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
