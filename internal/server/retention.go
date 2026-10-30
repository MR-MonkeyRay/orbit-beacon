package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"orbitbeacon/internal/logging"
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
	InboxNotifications     int64
	// BackupTasks counts processed task cleanup mutations, including secrets
	// cleared and deferred artifact unlinks as well as deleted task rows.
	BackupTasks       int64
	BackupOrphanFiles int64
	BackupLocalTemps  int64
	AlertEvents       int64
	HistoryBuckets    int64
	AuditEvents       int64
	EnrollmentTickets int64
	Sessions          int64
	TrafficRows       int64
	ProbeRows         int64
}

// RunRetention performs the bounded cleanup pass on the same hourly schedule
// used by the server process. The caller owns cancellation and waits for return.
func (h *handler) RunRetention(ctx context.Context) {
	ticker := time.NewTicker(RetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			result, err := h.CleanupRetention(ctx, now)
			if err != nil {
				if h.logger != nil {
					h.logger.LogAttrs(ctx, slog.LevelWarn, "retention_cleanup_failed", logging.ErrorAttrs(err, "cleanup_failed")...)
				}
				continue
			}
			if retentionChanged(result) && h.logger != nil {
				h.logger.LogAttrs(ctx, slog.LevelInfo, "retention_cleanup_complete", retentionAttrs(result)...)
			}
		}
	}
}

func retentionChanged(result RetentionResult) bool {
	return result.NotificationDeliveries != 0 || result.InboxNotifications != 0 ||
		result.BackupTasks != 0 || result.BackupOrphanFiles != 0 || result.BackupLocalTemps != 0 ||
		result.AlertEvents != 0 || result.HistoryBuckets != 0 || result.AuditEvents != 0 ||
		result.EnrollmentTickets != 0 || result.Sessions != 0 || result.TrafficRows != 0 || result.ProbeRows != 0
}

func retentionAttrs(result RetentionResult) []slog.Attr {
	return []slog.Attr{
		slog.Int64("notification_deliveries", result.NotificationDeliveries),
		slog.Int64("inbox_notifications", result.InboxNotifications),
		slog.Int64("backup_tasks", result.BackupTasks),
		slog.Int64("backup_orphan_files", result.BackupOrphanFiles),
		slog.Int64("backup_local_temps", result.BackupLocalTemps),
		slog.Int64("alert_events", result.AlertEvents),
		slog.Int64("history_buckets", result.HistoryBuckets),
		slog.Int64("audit_events", result.AuditEvents),
		slog.Int64("enrollment_tickets", result.EnrollmentTickets),
		slog.Int64("sessions", result.Sessions),
		slog.Int64("traffic_rows", result.TrafficRows),
		slog.Int64("probe_rows", result.ProbeRows),
	}
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
	if h.inbox != nil {
		result.InboxNotifications, err = cleanupRetentionBacklog(ctx, now.Add(-90*24*time.Hour), h.inbox.Cleanup)
		if err != nil {
			return result, fmt.Errorf("cleanup inbox notifications: %w", err)
		}
	}
	if h.backups != nil {
		// The hourly retention pass also runs on startup. Sweeps resume their
		// cursors, visit at most 500 entries each, and never traverse arbitrary
		// destinations from restored task history. Sweep before expiring tasks
		// so recorded local temporary files can still be located.
		result.BackupOrphanFiles, err = cleanupRetentionSweep(ctx, h.backups.CleanupOrphans)
		if err != nil {
			return result, fmt.Errorf("cleanup orphan exports: %w", err)
		}
		result.BackupLocalTemps, err = cleanupRetentionSweep(ctx, h.backups.CleanupLocalTemps)
		if err != nil {
			return result, fmt.Errorf("cleanup local backup temporary files: %w", err)
		}
		result.BackupTasks, err = cleanupRetentionBacklog(ctx, now, h.backups.Cleanup)
		if err != nil {
			return result, fmt.Errorf("cleanup backup tasks: %w", err)
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
	if h.nodes == nil {
		return result, fmt.Errorf("cleanup probe history: probe retention is unavailable")
	}
	result.ProbeRows, err = cleanupRetentionBacklog(ctx, now.Add(-7*24*time.Hour), h.nodes.CleanupProbeHistory)
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

func cleanupRetentionSweep(ctx context.Context, cleanup func(context.Context, int) (int64, error)) (int64, error) {
	budget, cancel := context.WithTimeout(ctx, retentionCatchupBudget)
	defer cancel()
	removed, err := cleanup(budget, retentionCatchupBatchSize)
	if err != nil && budget.Err() != nil && ctx.Err() == nil {
		return removed, nil
	}
	return removed, err
}
