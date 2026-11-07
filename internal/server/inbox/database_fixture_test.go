package inbox_test

import (
	"context"
	"database/sql"
	"orbitbeacon/internal/server/storage"
	"sync"
	"testing"
)

var mainHandlesMu sync.Mutex
var mainHandles = make(map[*sql.DB]*storage.Database)

func mainHandle(t *testing.T, raw *sql.DB) *storage.Database {
	t.Helper()
	mainHandlesMu.Lock()
	defer mainHandlesMu.Unlock()
	if db := mainHandles[raw]; db != nil {
		return db
	}
	var seq int
	var name, path string
	if err := raw.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenMain(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	mainHandles[raw] = db
	t.Cleanup(func() {
		mainHandlesMu.Lock()
		delete(mainHandles, raw)
		mainHandlesMu.Unlock()
		db.Close()
	})
	return db
}

func beginMainWrite(t *testing.T, raw *sql.DB, ctx context.Context) (*storage.Tx, error) {
	t.Helper()
	lease, err := mainHandle(t, raw).Acquire(ctx, storage.Writer)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { lease.Close() })
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		return nil, err
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx, nil
}
