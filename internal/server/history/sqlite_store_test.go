package history

import "testing"

// sqlitePools 仅供现有 SQL 断言访问 SQLite 连接池。
func sqlitePools(t *testing.T, s *Service) *sqliteStore {
	t.Helper()
	store, ok := s.store.(*sqliteStore)
	if !ok {
		t.Fatalf("history store is %T, want *sqliteStore", s.store)
	}
	return store
}
