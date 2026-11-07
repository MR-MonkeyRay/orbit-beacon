package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/backup"
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
	newer, err := storage.OpenWithMigrations(context.Background(), filepath.Join(dir, "metrics.db"), []storage.Migration{{Version: history.CurrentSchemaVersion + 1, SQL: `CREATE TABLE future_metrics (value TEXT)`}})
	if err != nil {
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
	stable, err := storage.OpenMain(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stable.Close()
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	if service := openMetricHistory(context.Background(), config.Server{DataDir: dir}, stable, logger, nil); service != nil {
		service.Close()
		t.Fatal("opened newer schema")
	}
	if log := output.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "history_disabled") || !strings.Contains(log, "history_open_failed") {
		t.Fatalf("log=%s", log)
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

func TestOpenMetricHistorySelectsBackend(t *testing.T) {
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
	logger := slog.New(slog.DiscardHandler)
	local := openMetricHistory(ctx, config.Server{DataDir: dir}, db, logger, nil)
	if local == nil {
		t.Fatal("history disabled")
	}
	if _, _, ok := local.DatabaseStats(); !ok {
		t.Fatal("default backend is not SQLite")
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	authorization := make(chan string, 2)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer remote.Close()
	service := openMetricHistory(ctx, config.Server{DataDir: dir, HistoryVictoriaMetricsURL: remote.URL, HistoryVictoriaMetricsToken: "history-token"}, db, logger, nil)
	if service == nil {
		t.Fatal("history disabled")
	}
	defer service.Close()
	if _, err := service.Query(ctx, "node", "cpu_percent", "1h", time.Now().UTC(), ""); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-authorization:
		if got != "Bearer history-token" {
			t.Fatalf("authorization=%q", got)
		}
	default:
		t.Fatal("remote backend not used")
	}
}

func TestVictoriaMetricsMonitoringBackupRejectsUnavailableSQLHistory(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "orbitbeacon-f2-backup-")
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
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	service := openMetricHistory(ctx, config.Server{DataDir: dir, HistoryVictoriaMetricsURL: "http://unreachable.invalid:8428"}, db, logger, nil)
	if service == nil {
		t.Fatalf("history disabled: %s", output.String())
	}
	if _, _, ok := service.DatabaseStats(); ok {
		t.Fatal("configured backend still uses SQLite pools")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = backup.CreateSnapshotPayloads(ctx, backup.SnapshotSources{Main: db}, dir, filepath.Join(dir, "orbitbeacon-f2-snapshot"), []backup.Category{backup.Monitoring}, time.Now)
	if err == nil || err.Error() != "history_unavailable" {
		t.Fatalf("VM monitoring backup=%v", err)
	}
}
