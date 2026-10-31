package server

import (
	"net/http"
	"time"
)

type probeView struct {
	Target     string   `json:"target"`
	Status     string   `json:"status"`
	LatencyMS  *float64 `json:"latency_ms"`
	Error      string   `json:"error"`
	StatusCode *int     `json:"status_code"`
	CheckedAt  *string  `json:"checked_at"`
	Uptime24H  *float64 `json:"uptime_24h"`
	Checks24H  int64    `json:"checks_24h"`
}

func (h *handler) nodeProbes(w http.ResponseWriter, r *http.Request, id, requestID string) {
	probes, err := h.readProbes(r, id)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Probes []probeView `json:"probes"`
	}{Probes: probes})
}

func (h *handler) readProbes(r *http.Request, id string) ([]probeView, error) {
	window := h.now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	observed, err := h.nodes.ObservedProbes(r.Context(), id, window)
	if err != nil {
		return nil, err
	}
	probes := make([]probeView, 0, len(observed))
	for _, stored := range observed {
		probe := probeView{
			Target: stored.Target, Status: stored.Status, LatencyMS: stored.LatencyMS,
			Error: stored.Error, StatusCode: stored.StatusCode, Uptime24H: stored.Uptime24H,
			Checks24H: stored.Checks24H,
		}
		if stored.CheckedAt != nil {
			formatted := stored.CheckedAt.Format(time.RFC3339Nano)
			probe.CheckedAt = &formatted
		}
		probes = append(probes, probe)
	}
	return probes, nil
}
