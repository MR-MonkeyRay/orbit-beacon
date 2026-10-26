package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestCleanupAuditEventsIsBoundedAndRetainsRecentEvents(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for _, event := range []struct{ id, created string }{{"old-a", old}, {"old-b", old}, {"recent", recent}} {
		if _, err := db.Exec(`INSERT INTO audit_events(id, action, created_at) VALUES (?, 'test', ?)`, event.id, event.created); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := NewService(db).CleanupAuditEvents(context.Background(), time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), 1)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want one bounded deletion", removed)
	}
	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE created_at < ?`, recent).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("old events remaining=%d, want one after bounded cleanup", remaining)
	}
	var recentCount int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE id='recent'`).Scan(&recentCount); err != nil {
		t.Fatal(err)
	}
	if recentCount != 1 {
		t.Fatal("recent audit event was deleted")
	}
	if _, err := NewService(db).CleanupAuditEvents(context.Background(), time.Now(), 0); err != ErrInvalidInput {
		t.Fatalf("limit 0 error=%v, want ErrInvalidInput", err)
	}
}
