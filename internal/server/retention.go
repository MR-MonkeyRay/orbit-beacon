package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"orbitbeacon/internal/logging"
)

const (
	// 审计保留期特意设置得足够长，以便运维调查；
	// 增量清理避免长期占用 SQLite 写锁。
	auditRetentionPeriod = 90 * 24 * time.Hour
	// 票据过期后 24 小时内仍接受注册重试。在该宽限期结束前保留令牌及其回执，
	// 然后再级联清理。
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
	// BackupTasks 统计任务清理的处理次数，包括清除敏感数据、
	// 删除文件失败后延迟重试，以及删除任务行。
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

// RunRetention 按服务器进程使用的同一每小时计划执行有界清理。
// 调用方拥有取消权并等待返回。
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
		// 每小时保留清理也在启动时运行。清扫从游标处继续，
		// 每次最多访问 500 个条目，且绝不遍历恢复的任务历史中的任意目标。
		// 先清扫再使任务过期，这样已记录的本地临时文件仍可被定位。
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
	// 会话 12 小时后过期；每小时几个有界批次即可跟上高频登录，
	// 且不会长期占用写锁。
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
	// 预算同样限制每个批次：缓慢的 DELETE 或忙等会在截止时间被取消，
	// 而不是超时运行。
	budget, cancel := context.WithTimeout(ctx, retentionCatchupBudget)
	defer cancel()
	var total int64
	for {
		// 每次调用独立提交短事务，在下一批次或时间预算检查前释放 SQLite 写锁。
		removed, err := cleanup(budget, cutoff, retentionCatchupBatchSize)
		total += removed
		if err != nil {
			if budget.Err() != nil && ctx.Err() == nil {
				// 预算耗尽：剩余部分等待下一轮。
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
