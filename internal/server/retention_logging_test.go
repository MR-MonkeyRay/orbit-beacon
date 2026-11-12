package server

import "testing"

func TestRetentionChangedIncludesEveryResultField(t *testing.T) {
	for i := range 15 {
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
		case 12:
			result.CommandRows = 1
		case 13:
			result.AvailabilityRows = 1
		case 14:
			result.AlertSilences = 1
		}
		if !retentionChanged(result) {
			t.Fatalf("field %d does not trigger summary", i)
		}
	}
}
