package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func testMainDatabases(t *testing.T) (*sql.DB, *storage.Database) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server.db")
	raw, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	stable, err := storage.OpenMain(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stable.Close() })
	return raw, stable
}
