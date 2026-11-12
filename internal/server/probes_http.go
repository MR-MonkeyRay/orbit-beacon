package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/validation"
)

func (h *handler) nodeProbeHistory(w http.ResponseWriter, r *http.Request, id, requestID string) {
	query, fields := parseQuery(r.URL.RawQuery, "must be a valid query string", map[string]queryParameter{
		"target": {repeated: "must be supplied exactly once"},
		"range":  {repeated: "must be supplied exactly once"},
	})
	target, requestedRange := query.Get("target"), query.Get("range")
	if err := v1.ValidateProbeTarget(target); err != nil {
		fields["target"] = "must be a valid probe target"
	}
	hours := 24
	switch requestedRange {
	case "24h":
	case "7d":
		hours = 168
	default:
		fields["range"] = "must be one of 24h, 7d"
	}
	if len(fields) > 0 {
		h.serviceError(w, &validation.Error{Fields: fields}, requestID)
		return
	}
	window := h.now().UTC().Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour)
	points, err := h.nodes.ProbeHistory(r.Context(), id, target, window)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Target      string                    `json:"target"`
		Range       string                    `json:"range"`
		Unit        string                    `json:"unit"`
		StepSeconds int                       `json:"step_seconds"`
		Points      []nodes.ProbeHistoryPoint `json:"points"`
	}{target, requestedRange, "ms", 3600, points})
}

type probeView struct {
	Target       string   `json:"target"`
	Status       string   `json:"status"`
	LatencyMS    *float64 `json:"latency_ms"`
	Error        string   `json:"error"`
	StatusCode   *int     `json:"status_code"`
	TLSExpiresAt *string  `json:"tls_expires_at"`
	CheckedAt    *string  `json:"checked_at"`
	Uptime24H    *float64 `json:"uptime_24h"`
	Checks24H    int64    `json:"checks_24h"`
}

func (h *handler) nodeProbes(w http.ResponseWriter, r *http.Request, id, requestID string) {
	probes, version, err := consoleSnapshot(h, r.Context(), func(context.Context) ([]probeView, error) { return h.readProbes(r, id) })
	if errors.Is(err, errSnapshotBusy) || err == nil && h.nodes.PublicVisibilityVersion() != version {
		fail(w, http.StatusServiceUnavailable, "snapshot_busy", "Node inventory is changing; retry later", requestID)
		return
	}
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
	latest, latestErr := h.ingest.Latest(r.Context(), id)
	if latestErr != nil && !errors.Is(latestErr, nodes.ErrNotFound) {
		return nil, latestErr
	}
	targets := make([]string, 0, len(latest.ProbeResults))
	for _, result := range latest.ProbeResults {
		targets = append(targets, result.Target)
	}
	observed, err := h.nodes.ObservedProbes(r.Context(), id, window, targets...)
	if err != nil {
		return nil, err
	}
	if latestErr == nil {
		stored := make(map[string]nodes.ObservedProbe, len(observed))
		for _, p := range observed {
			stored[p.Target] = p
		}
		observed = nil
		for _, result := range latest.ProbeResults {
			p := stored[result.Target]
			p.Target, p.Error, p.LatencyMS, p.StatusCode, p.TLSExpiresAt = result.Target, result.Error, result.LatencyMS, result.StatusCode, result.TLSExpiresAt
			p.Status, p.CheckedAt = "pending", nil
			if result.Error != "pending" {
				p.Status = "down"
				if result.OK {
					p.Status = "up"
				}
				at := result.CheckedAt
				p.CheckedAt = &at
			}
			observed = append(observed, p)
		}
	}
	sort.Slice(observed, func(i, j int) bool { return observed[i].Target < observed[j].Target })
	probes := make([]probeView, 0, len(observed))
	for _, stored := range observed {
		probe := probeView{
			Target: stored.Target, Status: stored.Status, LatencyMS: stored.LatencyMS,
			Error: stored.Error, StatusCode: stored.StatusCode, Uptime24H: stored.Uptime24H,
			Checks24H: stored.Checks24H,
		}
		if stored.TLSExpiresAt != nil {
			formatted := stored.TLSExpiresAt.Format(time.RFC3339Nano)
			probe.TLSExpiresAt = &formatted
		}
		if stored.CheckedAt != nil {
			formatted := stored.CheckedAt.Format(time.RFC3339Nano)
			probe.CheckedAt = &formatted
		}
		probes = append(probes, probe)
	}
	return probes, nil
}
