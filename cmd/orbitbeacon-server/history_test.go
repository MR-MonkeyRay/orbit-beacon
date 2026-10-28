package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/storage"
)

func TestNewerHistorySchemaDisablesHistoryWithoutBlockingRestore(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "data")
	if err := prepareDataDir(dir); err != nil {
		t.Fatal(err)
	}
	main, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := main.Close(); err != nil {
		t.Fatal(err)
	}
	newer, err := storage.OpenWithMigrations(context.Background(), filepath.Join(dir, "metrics.db"), []storage.Migration{{Version: history.CurrentSchemaVersion + 1, SQL: `CREATE TABLE future_metrics (value TEXT)`}})
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.Close(); err != nil {
		t.Fatal(err)
	}
	// Both backup and restore operate exclusively on the main database.
	archive := filepath.Join(parent, "backup.db")
	if err := backupDatabase(dir, archive); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dir, archive); err != nil {
		t.Fatalf("restore blocked by history: %v", err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	if service := openMetricHistory(context.Background(), dir, db, logger); service != nil {
		service.Close()
		t.Fatal("opened newer schema")
	}
	if log := output.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "history disabled") || !strings.Contains(log, "newer than supported") {
		t.Fatalf("log=%s", log)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("main DB unavailable: %v", err)
	}
	// The incompatible history file is retained, never deleted or silently reset.
	if _, err := os.Stat(filepath.Join(dir, "metrics.db")); err != nil {
		t.Fatal(err)
	}
	backup, err := storage.OpenReader(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('buckets','future_metrics')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("backup included history: count=%d err=%v", count, err)
	}
}
