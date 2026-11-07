package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLSaturationMetricsDoNotPublishStaleLocalWAL(t *testing.T) {
	fixture := storagetest.Start(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	databases, err := storage.OpenDatabases(context.Background(), config.DatabaseConfig{Primary: fixture.Endpoint("primary")}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer databases.Close()
	for _, name := range []string{"orbitbeacon.db-wal", "metrics.db-wal"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stale SQLite WAL"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body := NewSaturationMetrics(databases.Main, dir).exposition(&handler{})
	if strings.Contains(body, `orbitbeacon_db_wal_size_bytes{`) {
		t.Fatal("MySQL metrics published stale local SQLite WAL")
	}
	if !strings.Contains(body, `orbitbeacon_db_connections_in_use{database="main",role="writer"}`) {
		t.Fatal("active MySQL pools missing")
	}
}
