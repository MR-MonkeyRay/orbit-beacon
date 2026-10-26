package server

import (
	"context"
	"fmt"
	"time"
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
)

type RetentionResult struct {
	AuditEvents       int64
	EnrollmentTickets int64
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
	return RetentionResult{AuditEvents: removedAudit, EnrollmentTickets: removedTickets}, nil
}
