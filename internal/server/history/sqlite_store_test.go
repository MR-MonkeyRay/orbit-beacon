package history

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"orbitbeacon/internal/server/storage"
)

// 观察池只供测试准备与 SQL 断言，不作为生产服务的注入口。
func observeStore(t *testing.T, store *sqliteStore) *sql.DB {
	t.Helper()
	if raw, ok := storeObservers.Load(store); ok {
		return raw.(*sql.DB)
	}
	t.Fatal("history observer not registered")
	return nil
}

func registerStoreObserver(t *testing.T, store *sqliteStore, path string) {
	t.Helper()
	raw, err := storage.OpenHistorySQLite(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	storeObservers.Store(store, raw)
	t.Cleanup(func() {
		storeObservers.Delete(store)
		raw.Close()
	})
}

func observeHistory(t *testing.T, s *Service) *sql.DB {
	t.Helper()
	return observeStore(t, s.store.(*sqliteStore))
}

func registerHistoryObserver(t *testing.T, s *Service, dir string) {
	t.Helper()
	registerStoreObserver(t, s.store.(*sqliteStore), filepath.Join(dir, "metrics.db"))
}

var storeObservers sync.Map
