package server

import "testing"

func TestRetentionSummaryIncludesEveryResultField(t *testing.T) {
	result := RetentionResult{
		NotificationDeliveries: 1,
		InboxNotifications:     2,
		BackupTasks:            3,
		BackupOrphanFiles:      4,
		BackupLocalTemps:       5,
		AlertEvents:            6,
		HistoryBuckets:         7,
		AuditEvents:            8,
		EnrollmentTickets:      9,
		Sessions:               10,
		TrafficRows:            11,
		ProbeRows:              12,
	}
	attrs := retentionAttrs(result)
	if len(attrs) != 12 {
		t.Fatalf("summary has %d fields", len(attrs))
	}
	want := []string{
		"notification_deliveries", "inbox_notifications", "backup_tasks",
		"backup_orphan_files", "backup_local_temps", "alert_events",
		"history_buckets", "audit_events", "enrollment_tickets", "sessions",
		"traffic_rows", "probe_rows",
	}
	for i, key := range want {
		if attrs[i].Key != key || attrs[i].Value.Int64() != int64(i+1) {
			t.Fatalf("attribute %d = %s:%v", i, attrs[i].Key, attrs[i].Value)
		}
	}
}

func TestRetentionChangedIncludesEveryResultField(t *testing.T) {
	for i := range 12 {
		result := RetentionResult{}
		switch i {
		case 0:
			result.NotificationDeliveries = 1
		case 1:
			result.InboxNotifications = 1
		case 2:
			result.BackupTasks = 1
		case 3:
			result.BackupOrphanFiles = 1
		case 4:
			result.BackupLocalTemps = 1
		case 5:
			result.AlertEvents = 1
		case 6:
			result.HistoryBuckets = 1
		case 7:
			result.AuditEvents = 1
		case 8:
			result.EnrollmentTickets = 1
		case 9:
			result.Sessions = 1
		case 10:
			result.TrafficRows = 1
		case 11:
			result.ProbeRows = 1
		}
		if !retentionChanged(result) {
			t.Fatalf("field %d does not trigger summary", i)
		}
	}
}
