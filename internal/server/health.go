package server

import (
	"context"
	"net/http"
	"time"
)

type healthStatus struct {
	Status string `json:"status"`
}

func (h *handler) health(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.URL.Path == "/healthz/live" {
		writeJSON(w, http.StatusOK, healthStatus{Status: "ok"})
		return
	}
	if r.URL.Path != "/healthz/ready" {
		http.NotFound(w, r)
		return
	}
	if !h.readyState() || h.nodes == nil {
		writeJSON(w, http.StatusServiceUnavailable, healthStatus{Status: "not_ready"})
		return
	}
	pingContext, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	pingErr := h.nodes.Ping(pingContext)
	if pingErr != nil || !h.readyState() {
		writeJSON(w, http.StatusServiceUnavailable, healthStatus{Status: "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, healthStatus{Status: "ready"})
}

func (h *handler) readyState() bool {
	h.lifecycleMu.RLock()
	defer h.lifecycleMu.RUnlock()
	return h.ready
}
