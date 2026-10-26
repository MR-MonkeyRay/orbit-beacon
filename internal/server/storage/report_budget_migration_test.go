package storage_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestStartupRepairsLegacyReportBudgetAtomicallyOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(privateDirectory(t), "budget.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 4)
	for i := range paths {
		paths[i] = "/" + strings.Repeat("<", 1000) + string(rune('a'+i))
	}
	encoded, err := json.Marshal(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version=5"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO nodes(id,name,desired_revision,disk_paths,created_at) VALUES('bad','legacy',7,?,'2026-10-26T12:00:00Z'),('good','normal',3,'[\"/\"]','2026-10-26T12:00:00Z')", string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db, err = storage.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		var revision, goodRevision, events int
		var disks, interfaces string
		if err := db.QueryRow("SELECT desired_revision,disk_paths,interfaces FROM nodes WHERE id='bad'").Scan(&revision, &disks, &interfaces); err != nil {
			t.Fatal(err)
		}
		if revision != 8 || disks != "[]" || interfaces != "[]" {
			t.Fatalf("legacy repair not published: revision=%d disks=%s interfaces=%s", revision, disks, interfaces)
		}
		if err := db.QueryRow("SELECT desired_revision FROM nodes WHERE id='good'").Scan(&goodRevision); err != nil || goodRevision != 3 {
			t.Fatalf("normal node changed: revision=%d err=%v", goodRevision, err)
		}
		if err := db.QueryRow("SELECT count(*) FROM audit_events WHERE action='node.config.report_budget_repair' AND target_id='bad'").Scan(&events); err != nil || events != 1 {
			t.Fatalf("repair audit count=%d err=%v", events, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
