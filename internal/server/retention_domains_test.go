package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/storage"
)

func TestHistoryRetentionStillExpiresWhenMainCleanupFails(t *testing.T) {
	f := newConsoleLiveFixture(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := filepath.Join(dir, "history.db")
	raw, err := storage.OpenHistorySQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	now := time.Now().UTC()
	if _, err = raw.Exec("INSERT INTO buckets VALUES('not-on-main','cpu',3600,?,7,7,1)", now.Add(-100*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenHistory(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	f.h.history = history.OpenWithDatabase(db, f.h.nodes.Database(), history.Options{})
	defer f.h.history.Close()
	if err = f.h.nodes.Database().Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.CleanupRetention(ctx, now); err == nil {
		t.Fatal("main failure was hidden")
	}
	var remaining int
	if err = raw.QueryRow("SELECT count(*) FROM buckets").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("main failure prevented independent history TTL: remaining=%d err=%v", remaining, err)
	}
}
