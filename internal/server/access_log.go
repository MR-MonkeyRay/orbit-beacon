package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// responseLog records bytes actually written, without inspecting response bodies
// or credentials. Unwrap preserves ResponseController support.
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
	// Informational responses do not commit the final response status.
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
		route := requestRoute(r.URL.Path)
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

// Only known route templates enter logs or labels. Unknown paths, arbitrary
// filenames, and malformed IDs may contain secrets, so never echo them back.
func requestRoute(p string) string {
	if healthProbe(p) {
		return p
	}
	if len(routeMethods(p)) != 0 {
		parts := strings.Split(p, "/")
		if len(parts) >= 5 && parts[1] == "api" && parts[2] == "v1" {
			switch parts[3] {
			case "nodes", "users", "groups", "enrollments", "alert-rules", "notification-channels":
				if parts[4] != "batch" {
					parts[4] = "{id}"
				}
			}
		}
		return strings.Join(parts, "/")
	}
	switch p {
	case "/", "/login", "/overview", "/nodes", "/alerts", "/settings":
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
