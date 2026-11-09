package main

import (
	"context"
	"database/sql"
	"orbitbeacon/internal/server/storage"
	"path/filepath"
	"testing"
)

func commandMainHandle(t *testing.T, raw *sql.DB) *storage.Database {
	t.Helper()
	var seq int
	var name, path string
	if err := raw.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	stable, err := storage.OpenMain(context.Background(), filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stable.Close() })
	return stable
}
