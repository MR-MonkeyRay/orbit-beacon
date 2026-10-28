package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenMigratesFreshDatabaseAndPreservesDataOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(privateDirectory(t), "orbitbeacon.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range []string{
		"users", "sessions", "nodes", "groups", "node_tags", "enrollment_tokens",
		"enrollment_claims", "settings", "audit_events", "schema_migrations",
		"traffic_hourly", "traffic_daily", "probe_latest", "probe_hourly",
	} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("migration did not create table %q", table)
		}
	}
	var applied int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Fatal("no migration was recorded")
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE regression_sentinel (value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO regression_sentinel(value) VALUES (?)", "survives restart"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRowContext(ctx, "SELECT value FROM regression_sentinel").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "survives restart" {
		t.Errorf("stored value = %q", value)
	}
	var after int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != applied {
		t.Errorf("reopening reapplied migrations: before=%d after=%d", applied, after)
	}
}

func TestOpenConfiguresDurableSQLiteConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := storage.Open(ctx, filepath.Join(privateDirectory(t), "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal mode = %q, want wal", journal)
	}
	// Hold connections simultaneously so per-connection options cannot be satisfied
	// only on the connection that happened to apply the migrations.
	var connections []*sql.Conn
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := range 2 {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		for pragma, want := range map[string]int{
			"foreign_keys": 1, "busy_timeout": 5000, "synchronous": 2, "mmap_size": 0,
		} {
			var got int
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("connection %d %s = %d, want %d", i, pragma, got, want)
			}
		}
	}
}

func TestOpenReaderUsesReadOnlyPoolAndDoesNotCreateDatabase(t *testing.T) {
	ctx := context.Background()
	dir := privateDirectory(t)
	path := filepath.Join(dir, "orbitbeacon.db")
	reader, err := storage.OpenReader(ctx, path)
	if reader != nil {
		_ = reader.Close()
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenReader missing database error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("OpenReader created missing database: %v", err)
	}

	writer, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, "CREATE TABLE reader_sentinel (value TEXT NOT NULL); INSERT INTO reader_sentinel VALUES ('visible')"); err != nil {
		t.Fatal(err)
	}
	reader, err = storage.OpenReader(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got := reader.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("reader MaxOpenConnections = %d, want 4", got)
	}
	var value string
	if err := reader.QueryRowContext(ctx, "SELECT value FROM reader_sentinel").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "visible" {
		t.Fatalf("reader value = %q", value)
	}
	if _, err := reader.ExecContext(ctx, "INSERT INTO reader_sentinel VALUES ('forbidden')"); err == nil {
		t.Fatal("read-only pool accepted a write")
	}
	var connections []*sql.Conn
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := range 4 {
		conn, err := reader.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		var queryOnly, busyTimeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if queryOnly != 1 || busyTimeout != 5000 {
			t.Fatalf("reader connection %d pragmas query_only=%d busy_timeout=%d", i, queryOnly, busyTimeout)
		}
	}
}

func TestOpenRejectsURIPaths(t *testing.T) {
	for _, path := range []string{
		":memory:", "file::memory:?cache=shared", "file:database.db?mode=memory",
		"https://example.test/database", "postgres://user:password@example.test/database",
	} {
		t.Run(path, func(t *testing.T) {
			db, err := storage.Open(context.Background(), path)
			if db != nil {
				_ = db.Close()
			}
			if err == nil {
				t.Fatal("accepted a URI or nonpersistent SQLite path")
			}
		})
	}
}

func TestMigrationFailureDoesNotRecordOrPartiallyApplyMigration(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(privateDirectory(t), "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var before int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&before); err != nil {
		t.Fatal(err)
	}
	err = storage.Migrate(ctx, db, []storage.Migration{{
		Version: 1000000,
		SQL:     "CREATE TABLE rollback_sentinel (id INTEGER); INSERT INTO missing_migration_table VALUES (1);",
	}})
	if err == nil {
		t.Fatal("broken migration succeeded")
	}
	var partial, after int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'rollback_sentinel'").Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if partial != 0 || after != before {
		t.Errorf("failed migration was not atomic: partial tables=%d, migrations before=%d after=%d", partial, before, after)
	}
	if err := storage.Migrate(ctx, db, []storage.Migration{{Version: 1000000, SQL: "CREATE TABLE rollback_sentinel (id INTEGER);"}}); err != nil {
		t.Fatalf("retrying a repaired, previously failed migration: %v", err)
	}
}
