package server

import (
	"context"
	"net/http"
	"time"
)

type healthStatus struct {
	Status string `json:"status"`
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not supported for this route", "")
		return
	}
	if r.URL.Path == "/healthz/live" {
		writeJSON(w, http.StatusOK, healthStatus{Status: "ok"})
		return
	}
	if r.URL.Path != "/healthz/ready" {
		http.NotFound(w, r)
		return
	}
	if !h.readyState() || h.nodes == nil || h.nodes.Database() == nil {
		writeJSON(w, http.StatusServiceUnavailable, healthStatus{Status: "not_ready"})
		return
	}
	pingContext, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	pingErr := h.nodes.Database().PingContext(pingContext)
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
