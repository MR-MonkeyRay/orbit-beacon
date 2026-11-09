package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"orbitbeacon/internal/config"
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
	mainObservers[db] = raw
	t.Cleanup(func() {
		mainHandlesMu.Lock()
		delete(mainHandles, raw)
		delete(mainObservers, db)
		mainHandlesMu.Unlock()
		db.Close()
	})
	return db
}

var (
	mainHandlesMu sync.Mutex
	mainHandles   = make(map[*sql.DB]*storage.Database)
	mainObservers = make(map[*storage.Database]*sql.DB)
)

func observeMain(t *testing.T, service *Service) *sql.DB {
	t.Helper()
	mainHandlesMu.Lock()
	defer mainHandlesMu.Unlock()
	raw := mainObservers[service.db]
	if raw == nil {
		t.Fatal("main observer not registered")
	}
	return raw
}

func snapshotSources(t *testing.T, dir string) SnapshotSources {
	t.Helper()
	main, err := storage.OpenMain(t.Context(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { main.Close() })
	sources := SnapshotSources{Main: main}
	path := filepath.Join(dir, "metrics.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return sources
	}
	sources.History, err = storage.OpenHistory(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	history := sources.History
	t.Cleanup(func() { history.Close() })
	return sources
}

func restoreConfig(dir string) config.Server { return config.Server{DataDir: dir} }

// writableRestoreFixture 只用于准备测试拥有的可信 SQLite 状态；生产 reader 保持只读。
func writableRestoreFixture(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := storage.Open(t.Context(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
