package server

import (
	"context"
	"fmt"
	"time"

	"orbitbeacon/internal/timekey"
)

const (
	// Audit retention is deliberately long enough for operational investigation;
	// cleanup is incremental so it cannot monopolize the SQLite writer.
	auditRetentionPeriod = 90 * 24 * time.Hour
	// Enrollment retries are accepted for 24 hours after ticket expiry. Keep the
	// token and its receipt for that exact grace period before cascading cleanup.
	enrollmentRetentionPeriod = 24 * time.Hour
	RetentionInterval         = time.Hour
	retentionBatchSize        = 128
	retentionSessionBatches   = 16
	retentionCatchupBatchSize = 500
	retentionCatchupBudget    = 2 * time.Second
)

type RetentionResult struct {
	NotificationDeliveries int64
	AlertEvents            int64
	HistoryBuckets         int64
	AuditEvents            int64
	EnrollmentTickets      int64
	Sessions               int64
	TrafficRows            int64
	ProbeRows              int64
}

func (h *handler) CleanupRetention(ctx context.Context, now time.Time) (RetentionResult, error) {
	if h.management == nil || h.enrollments == nil {
		return RetentionResult{}, fmt.Errorf("retention services are unavailable")
	}
	now = now.UTC()
	removedAudit, err := h.management.CleanupAuditEvents(ctx, now.Add(-auditRetentionPeriod), retentionBatchSize)
	if err != nil {
		return RetentionResult{}, fmt.Errorf("cleanup audit events: %w", err)
	}
	removedTickets, err := h.enrollments.CleanupExpiredTickets(ctx, now.Add(-enrollmentRetentionPeriod), retentionBatchSize)
	if err != nil {
		return RetentionResult{AuditEvents: removedAudit}, fmt.Errorf("cleanup enrollment tickets: %w", err)
	}
	result := RetentionResult{AuditEvents: removedAudit, EnrollmentTickets: removedTickets}
	if h.alerts != nil {
		result.AlertEvents, err = cleanupRetentionBacklog(ctx, now.Add(-90*24*time.Hour), h.alerts.Cleanup)
		if err != nil {
			return result, fmt.Errorf("cleanup alert events: %w", err)
		}
	}
	if h.notifications != nil {
		result.NotificationDeliveries, err = cleanupRetentionBacklog(ctx, now, h.notifications.Cleanup)
		if err != nil {
			return result, fmt.Errorf("cleanup notification deliveries: %w", err)
		}
	}
	// Sessions expire after 12 hours; a few bounded batches per hour keep up
	// with heavy login traffic without monopolizing the writer.
	for range retentionSessionBatches {
		removed, err := h.auth.CleanupExpiredSessions(ctx)
		result.Sessions += removed
		if err != nil {
			return result, fmt.Errorf("cleanup sessions: %w", err)
		}
		if removed < retentionBatchSize {
			break
		}
	}

	if h.traffic != nil {
		result.TrafficRows, err = cleanupRetentionBacklog(ctx, now, h.traffic.Cleanup)
		if err != nil {
			return result, fmt.Errorf("cleanup traffic: %w", err)
		}
	}
	result.ProbeRows, err = cleanupRetentionBacklog(ctx, now.Add(-7*24*time.Hour), h.cleanupProbeHourly)
	if err != nil {
		return result, fmt.Errorf("cleanup probe history: %w", err)
	}
	if h.history != nil {
		result.HistoryBuckets, err = h.history.Cleanup(ctx, now)
		if err != nil {
			return result, fmt.Errorf("cleanup metric history: %w", err)
		}
	}
	return result, nil
}

func (h *handler) cleanupProbeHourly(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if h.nodes == nil || limit < 1 {
		return 0, fmt.Errorf("probe retention is unavailable")
	}
	result, err := h.nodes.Database().ExecContext(ctx, `DELETE FROM probe_hourly
WHERE (node_id,target,hour) IN (
 SELECT node_id,target,hour FROM probe_hourly WHERE hour < ? ORDER BY hour LIMIT ?
)`, timekey.Format(cutoff), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func cleanupRetentionBacklog(ctx context.Context, cutoff time.Time, cleanup func(context.Context, time.Time, int) (int64, error)) (int64, error) {
	// The budget bounds each batch too: a slow DELETE or busy wait is cancelled
	// at the deadline rather than overrunning it.
	budget, cancel := context.WithTimeout(ctx, retentionCatchupBudget)
	defer cancel()
	var total int64
	for {
		// Each call commits its own short batch, releasing the SQLite writer
		// before the next batch or the time-budget check.
		removed, err := cleanup(budget, cutoff, retentionCatchupBatchSize)
		total += removed
		if err != nil {
			if budget.Err() != nil && ctx.Err() == nil {
				// Out of budget: the remainder waits for the next pass.
				return total, nil
			}
			return total, err
		}
		if removed == 0 || budget.Err() != nil {
			return total, nil
		}
	}
}
