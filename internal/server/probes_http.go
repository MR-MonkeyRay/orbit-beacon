package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"orbitbeacon/internal/server/nodes"
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
	window := h.now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	// One read snapshot binds the target list and observations to the same
	// credential. Rotation cannot turn old targets into a new identity's probes.
	rows, err := h.nodes.Database().QueryContext(r.Context(), `
		SELECT nodes.id, selected.value, latest.ok, latest.latency_ms, latest.error,
		       latest.status_code, latest.checked_at,
		       coalesce(sum(hourly.checks),0), coalesce(sum(hourly.failures),0)
		FROM nodes
		LEFT JOIN json_each(CASE WHEN length(nodes.token_hash)>0 AND nodes.run_generation!='0' AND nodes.report_session_id!='' THEN nullif(nodes.agent_runtime,'') ELSE NULL END, '$.probe_targets') AS selected ON true
		LEFT JOIN probe_latest AS latest ON latest.node_id=nodes.id AND latest.target=selected.value
		LEFT JOIN probe_hourly AS hourly ON hourly.node_id=nodes.id AND hourly.target=selected.value AND hourly.hour>=?
		WHERE nodes.id=? GROUP BY nodes.id,selected.key ORDER BY selected.key`, timekey.Format(window), id)
	if err != nil {
		return nil, fmt.Errorf("read observed probes: %w", err)
	}
	defer rows.Close()
	probes := []probeView{}
	found := false
	for rows.Next() {
		found = true
		var nodeID string
		var target, failure, checked sql.NullString
		var ok sql.NullBool
		var latency sql.NullFloat64
		var status sql.NullInt64
		var checks, failures int64
		if err := rows.Scan(&nodeID, &target, &ok, &latency, &failure, &status, &checked, &checks, &failures); err != nil {
			return nil, err
		}
		if !target.Valid {
			continue
		}
		probe := probeView{Target: target.String, Status: "pending", Error: failure.String, Checks24H: checks}
		if latency.Valid {
			probe.LatencyMS = &latency.Float64
		}
		if status.Valid {
			value := int(status.Int64)
			probe.StatusCode = &value
		}
		if checked.Valid {
			probe.Status = "down"
			if ok.Bool {
				probe.Status = "up"
			}
			stamp, err := timekey.Parse(checked.String)
			if err != nil {
				return nil, fmt.Errorf("parse latest probe timestamp: %w", err)
			}
			formatted := stamp.Format(time.RFC3339Nano)
			probe.CheckedAt = &formatted
		}
		if checks > 0 {
			uptime := 100 * float64(checks-failures) / float64(checks)
			probe.Uptime24H = &uptime
		}
		probes = append(probes, probe)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nodes.ErrNotFound
	}
	return probes, nil
}
