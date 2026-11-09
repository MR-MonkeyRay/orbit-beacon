package history

import (
	"database/sql"
	"sync"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func mainHandle(t *testing.T, raw *sql.DB) *storage.Database {
	t.Helper()
	if raw == nil {
		return nil
	}
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

var (
	mainHandlesMu sync.Mutex
	mainHandles   = make(map[*sql.DB]*storage.Database)
)
