package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"orbitbeacon/internal/timekey"
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
	cfg, err := h.nodes.GetConfig(r.Context(), id)
	if err != nil {
		return nil, err
	}
	probes := make([]probeView, len(cfg.ProbeTargets))
	window := h.now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	for index, target := range cfg.ProbeTargets {
		probe := probeView{Target: target, Status: "pending"}
		var ok int
		var checkedAt string
		err := h.nodes.Database().QueryRowContext(r.Context(), `SELECT ok,latency_ms,error,status_code,checked_at FROM probe_latest WHERE node_id=? AND target=?`, id, target).Scan(&ok, &probe.LatencyMS, &probe.Error, &probe.StatusCode, &checkedAt)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("read latest probe: %w", err)
		}
		if err == nil {
			probe.Status = "down"
			if ok != 0 {
				probe.Status = "up"
			}
			stamp, parseErr := timekey.Parse(checkedAt)
			if parseErr != nil {
				return nil, fmt.Errorf("parse latest probe timestamp: %w", parseErr)
			}
			formatted := stamp.Format(time.RFC3339Nano)
			probe.CheckedAt = &formatted
		}
		var failures int64
		if err := h.nodes.Database().QueryRowContext(r.Context(), `SELECT coalesce(sum(checks),0),coalesce(sum(failures),0) FROM probe_hourly WHERE node_id=? AND target=? AND hour>=?`, id, target, timekey.Format(window)).Scan(&probe.Checks24H, &failures); err != nil {
			return nil, fmt.Errorf("read probe uptime: %w", err)
		}
		if probe.Checks24H > 0 {
			uptime := 100 * float64(probe.Checks24H-failures) / float64(probe.Checks24H)
			probe.Uptime24H = &uptime
		}
		probes[index] = probe
	}
	return probes, nil
}
