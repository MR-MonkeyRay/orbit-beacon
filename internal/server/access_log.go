package server

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// responseLog 记录实际写出的字节数，不检查响应体或凭据。
// Unwrap 保留对 ResponseController 的支持。
type responseLog struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseLog) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	// 信息性响应不提交最终的响应状态。
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseLog) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (h *handler) observeRequest(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	start := time.Now()
	response := &responseLog{ResponseWriter: w}
	if h.metrics != nil {
		h.metrics.inflight.Add(1)
	}
	defer func() {
		panicValue := recover()
		if h.metrics != nil {
			h.metrics.inflight.Add(-1)
		}
		status := response.status
		if status == 0 {
			status = http.StatusOK
			if panicValue != nil {
				status = http.StatusInternalServerError
			}
		}
		duration := time.Since(start)
		route := r.Pattern
		if route == "" {
			route = staticRequestRoute(r.URL.Path)
		}
		if h.metrics != nil {
			h.metrics.observe(r.Method, route, status, duration, r.URL.Path)
		}
		if h.accessLog && (!healthProbe(r.URL.Path) || status >= 500) {
			h.logger.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
				slog.String("method", r.Method), slog.String("route", route),
				slog.Int("status", status), slog.Int64("bytes", response.bytes),
				slog.Float64("duration_ms", float64(duration)/float64(time.Millisecond)),
				slog.String("request_id", response.Header().Get("X-Request-ID")),
				slog.String("client_addr", h.clientAddr(r)))
		}
		if panicValue != nil {
			panic(panicValue) // Preserve net/http panic handling.
		}
	}()
	next(response, r)
}

func healthProbe(p string) bool { return p == "/healthz/live" || p == "/healthz/ready" }

// 只有已知路由模板进入日志或标签。未知路径、任意文件名和格式错误的 ID
// 可能包含机密，因此绝不回显它们。
func requestRoute(p string) string {
	if entry := apiRoutes.match(&http.Request{URL: &url.URL{Path: p}}); entry != nil {
		return entry.label
	}
	return staticRequestRoute(p)
}

func staticRequestRoute(p string) string {
	switch p {
	case "/", "/login", "/change-password", "/overview", "/nodes", "/alerts", "/settings":
		return p
	}
	if strings.HasPrefix(p, "/nodes/") {
		return "/nodes/{id}"
	}
	if strings.HasPrefix(p, "/_app/immutable/") {
		return "/_app/immutable/{asset}"
	}
	return "unmatched"
}
