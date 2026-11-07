package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/storage"
)

func TestNewerHistorySchemaDisablesHistoryWithoutChangingFiles(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "data")
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	main, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := main.Close(); err != nil {
		t.Fatal(err)
	}
	newer, err := storage.OpenHistorySQLite(context.Background(), filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newer.Exec("UPDATE schema_migrations SET version=?", history.CurrentSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := newer.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	databases, openErr := storage.OpenDatabases(context.Background(), config.DatabaseConfig{}, dir)
	if openErr == nil || databases == nil || databases.Main == nil || databases.History != nil {
		t.Fatalf("incompatible history did not leave main usable: %+v/%v", databases, openErr)
	}
	defer databases.Close()
	if service := openMetricHistory(databases.Main, databases.History, slog.New(slog.DiscardHandler), nil); service != nil {
		service.Close()
		t.Fatal("opened newer schema")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("main DB unavailable: %v", err)
	}
	// 不兼容的历史文件予以保留，绝不删除或静默重置。
	if _, err := os.Stat(filepath.Join(dir, "metrics.db")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMetricHistoryUsesOpenedSQLSource(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "orbitbeacon-f2-backend-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenMain(ctx, filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	metrics, err := storage.OpenHistory(ctx, filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	local := openMetricHistory(db, metrics, logger, nil)
	if local == nil {
		t.Fatal("history disabled")
	}
	if _, _, ok := local.DatabaseStats(); !ok {
		t.Fatal("default backend is not SQLite")
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
}
