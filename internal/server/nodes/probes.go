package nodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

type ObservedProbe struct {
	Target       string
	Status       string
	LatencyMS    *float64
	Error        string
	StatusCode   *int
	TLSExpiresAt *time.Time
	CheckedAt    *time.Time
	Uptime24H    *float64
	Checks24H    int64
}

type ProbeHistoryPoint struct {
	Time     time.Time `json:"t"`
	Avg      *float64  `json:"avg"`
	Max      *float64  `json:"max"`
	Checks   int64     `json:"checks"`
	Failures int64     `json:"failures"`
}

func (s *Service) ProbeHistory(ctx context.Context, id, target string, window time.Time) ([]ProbeHistoryPoint, error) {
	// 一次读取快照把节点是否存在、当前身份与保留的目标历史绑定在一起。
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()

	rows, err := lease.QueryContext(ctx, `
		SELECT nodes.id, hourly.hour, hourly.checks, hourly.failures,
		       hourly.latency_sum_ms, hourly.latency_max_ms
		FROM nodes
		LEFT JOIN probe_hourly AS hourly ON hourly.node_id=nodes.id AND hourly.target=? AND hourly.hour>=?
         AND length(nodes.token_hash)>0 AND nodes.run_generation!='0' AND nodes.report_session_id!=''
		WHERE nodes.id=? ORDER BY hourly.hour`, target, timekey.Format(window), id)
	if err != nil {
		return nil, fmt.Errorf("read probe history: %w", err)
	}
	defer rows.Close()
	points := []ProbeHistoryPoint{}
	found := false
	for rows.Next() {
		found = true
		var nodeID string
		var hour sql.NullString
		var checks, failures sql.NullInt64
		var sum, maximum sql.NullFloat64
		if err := rows.Scan(&nodeID, &hour, &checks, &failures, &sum, &maximum); err != nil {
			return nil, err
		}
		if !hour.Valid {
			continue
		}
		stamp, err := timekey.Parse(hour.String)
		if err != nil {
			return nil, fmt.Errorf("parse probe history hour: %w", err)
		}
		point := ProbeHistoryPoint{Time: stamp, Checks: checks.Int64, Failures: failures.Int64}
		if successes := point.Checks - point.Failures; successes > 0 {
			avg := sum.Float64 / float64(successes)
			point.Avg, point.Max = &avg, &maximum.Float64
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return points, nil
}

func (s *Service) ObservedProbes(ctx context.Context, id string, window time.Time, targets ...string) ([]ObservedProbe, error) {
	// 一次读取快照把目标列表和观测结果绑定到同一凭据。
	// 轮换不会把旧目标变成新身份的探测项。
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()

	if len(targets) > v1.MaxProbeTargets {
		return nil, errors.New("too many probe targets")
	}
	filter := ""
	args := []any{}
	if len(targets) > 0 {
		filter = " AND latest.target IN (" + strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",") + ")"
		for _, target := range targets {
			args = append(args, target)
		}
	}
	args = append(args, timekey.Format(window), id, v1.MaxProbeTargets)
	rows, err := lease.QueryContext(ctx, `
        SELECT nodes.id, latest.target, latest.ok, latest.latency_ms, latest.error,
		       latest.status_code, latest.checked_at, latest.tls_expires_at,
		       coalesce(sum(hourly.checks),0), coalesce(sum(hourly.failures),0)
		FROM nodes
		LEFT JOIN probe_latest AS latest ON latest.node_id=nodes.id
          AND length(nodes.token_hash)>0 AND nodes.run_generation!='0' AND nodes.report_session_id!=''`+filter+`
		LEFT JOIN probe_hourly AS hourly ON hourly.node_id=nodes.id AND hourly.target=latest.target AND hourly.hour>=?
		WHERE nodes.id=? GROUP BY nodes.id,latest.target,latest.ok,latest.latency_ms,latest.error,latest.status_code,latest.checked_at,latest.tls_expires_at ORDER BY max(latest.checked_at) DESC,latest.target LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read observed probes: %w", err)
	}
	defer rows.Close()
	probes := []ObservedProbe{}
	found := false
	for rows.Next() {
		found = true
		var nodeID string
		var target, failure, checked, expiry sql.NullString
		var ok sql.NullBool
		var latency sql.NullFloat64
		var status sql.NullInt64
		var checks, failures int64
		if err := rows.Scan(&nodeID, &target, &ok, &latency, &failure, &status, &checked, &expiry, &checks, &failures); err != nil {
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
		if expiry.Valid {
			stamp, err := timekey.Parse(expiry.String)
			if err != nil {
				return nil, fmt.Errorf("parse probe TLS expiry: %w", err)
			}
			probe.TLSExpiresAt = &stamp
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
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return 0, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count, err := storage.DeleteBatch(ctx, tx, `SELECT node_id,target,hour FROM probe_hourly WHERE hour<? ORDER BY hour LIMIT ?`, `DELETE FROM probe_hourly WHERE node_id=? AND target=? AND hour=?`, timekey.Format(cutoff), limit)
	if err != nil {
		return 0, err
	}
	if count < int64(limit) {
		latestCount, err := storage.DeleteBatch(ctx, tx, `SELECT node_id,target FROM probe_latest WHERE checked_at<? ORDER BY checked_at,target LIMIT ?`, `DELETE FROM probe_latest WHERE node_id=? AND target=?`, timekey.Format(cutoff), limit-int(count))
		if err != nil {
			return 0, err
		}
		count += latestCount
	}
	return count, tx.Commit()
}

func (s *Service) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("node database is unavailable")
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	var present int
	return lease.QueryRowContext(ctx, "SELECT 1").Scan(&present)
}
