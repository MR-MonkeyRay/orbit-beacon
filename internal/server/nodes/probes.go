package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"orbitbeacon/internal/timekey"
)

type ObservedProbe struct {
	Target     string
	Status     string
	LatencyMS  *float64
	Error      string
	StatusCode *int
	CheckedAt  *time.Time
	Uptime24H  *float64
	Checks24H  int64
}

func (s *Service) ObservedProbes(ctx context.Context, id string, window time.Time) ([]ObservedProbe, error) {
	// One read snapshot binds the target list and observations to the same
	// credential. Rotation cannot turn old targets into a new identity's probes.
	rows, err := s.db.QueryContext(ctx, `
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
	probes := []ObservedProbe{}
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
		probe := ObservedProbe{Target: target.String, Status: "pending", Error: failure.String, Checks24H: checks}
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
			probe.CheckedAt = &stamp
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
		return nil, ErrNotFound
	}
	return probes, nil
}

func (s *Service) CleanupProbeHistory(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if s == nil || s.db == nil || limit < 1 {
		return 0, errors.New("probe retention is unavailable")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM probe_hourly
WHERE (node_id,target,hour) IN (
 SELECT node_id,target,hour FROM probe_hourly WHERE hour < ? ORDER BY hour LIMIT ?
)`, timekey.Format(cutoff), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Service) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("node database is unavailable")
	}
	return s.db.PingContext(ctx)
}
