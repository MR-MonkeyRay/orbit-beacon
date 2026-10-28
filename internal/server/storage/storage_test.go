package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func TestOpenInitializesFreshDatabaseAndPreservesDataOnReopen(t *testing.T) {
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
	if applied != 1 {
		t.Fatalf("ledger rows = %d, want exactly one current version", applied)
	}
	var version int
	if err := db.QueryRowContext(ctx, "SELECT version FROM schema_migrations").Scan(&version); err != nil || version != storage.CurrentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var obsolete int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('sessions') WHERE name='csrf_hash'`).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatalf("obsolete session field=%d err=%v", obsolete, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name='api_tokens'`).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatalf("obsolete API tokens=%d err=%v", obsolete, err)
	}
	for _, field := range []struct{ table, column string }{{"users", "role"}, {"inbox_notifications", "audience"}, {"groups", "desired_config"}, {"nodes", "desired_revision"}, {"nodes", "applied_revision"}, {"nodes", "config_error"}, {"nodes", "report_interval_seconds"}, {"nodes", "disk_interval_seconds"}, {"nodes", "interfaces"}, {"nodes", "disk_paths"}, {"nodes", "probe_targets"}} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, field.table, field.column).Scan(&obsolete); err != nil || obsolete != 0 {
			t.Fatalf("obsolete %s.%s=%d err=%v", field.table, field.column, obsolete, err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name='inbox_notifications_audience'`).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatalf("obsolete audience index=%d err=%v", obsolete, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES('roleless','roleless','hash','now')`); err != nil {
		t.Fatal(err)
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

func TestOpenRejectsIncompatibleLedgerWithoutChangingData(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		ledger string
	}{
		{"missing", ""},
		{"empty", "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);"},
		{"old", "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations VALUES(16,'original');"},
		{"intermediate", "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations VALUES(17,'original');"},
		{"previous-current", "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations VALUES(18,'original');"},
		{"mixed", "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations VALUES(1,'original'),(16,'original');"},
		{"mixed-current", fmt.Sprintf("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations VALUES(1,'original'),(%d,'original');", storage.CurrentSchemaVersion)},
		{"future", fmt.Sprintf("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations VALUES(%d,'original');", storage.CurrentSchemaVersion+1)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(privateDirectory(t), "incompatible.db")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`CREATE TABLE sentinel(value TEXT NOT NULL); INSERT INTO sentinel VALUES('untouched');` + fixture.ledger); err != nil {
				t.Fatal(err)
			}
			if fixture.name == "old" || fixture.name == "intermediate" {
				if _, err := db.Exec(`CREATE TABLE users(id TEXT PRIMARY KEY,username TEXT,password_hash TEXT,role TEXT,created_at TEXT); INSERT INTO users VALUES('original','original','hash','viewer','original')`); err != nil {
					t.Fatal(err)
				}
			}
			var before int
			if err = db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			var ledgerBefore string
			if fixture.ledger != "" {
				if err = db.QueryRow(`SELECT coalesce(group_concat(version || ':' || applied_at),'') FROM (SELECT * FROM schema_migrations ORDER BY version)`).Scan(&ledgerBefore); err != nil {
					t.Fatal(err)
				}
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := storage.Open(context.Background(), path)
			if opened != nil {
				opened.Close()
			}
			want := storage.ErrSchemaIncompatible
			if fixture.name == "future" {
				want = storage.ErrSchemaTooNew
			}
			if !errors.Is(err, want) {
				t.Fatalf("database error=%v, want %v", err, want)
			}
			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var value string
			var after int
			if err = db.QueryRow(`SELECT value FROM sentinel`).Scan(&value); err != nil || value != "untouched" {
				t.Fatalf("sentinel=%q err=%v", value, err)
			}
			if fixture.name == "old" || fixture.name == "intermediate" {
				var role string
				if err := db.QueryRow(`SELECT role FROM users WHERE id='original'`).Scan(&role); err != nil || role != "viewer" {
					t.Fatalf("old account changed: role=%q err=%v", role, err)
				}
			}
			if err = db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&after); err != nil || after != before {
				t.Fatalf("schema changed: before=%d after=%d err=%v", before, after, err)
			}
			if fixture.ledger != "" {
				var ledgerAfter string
				if err = db.QueryRow(`SELECT coalesce(group_concat(version || ':' || applied_at),'') FROM (SELECT * FROM schema_migrations ORDER BY version)`).Scan(&ledgerAfter); err != nil || ledgerAfter != ledgerBefore {
					t.Fatalf("ledger changed: before=%q after=%q err=%v", ledgerBefore, ledgerAfter, err)
				}
			}
		})
	}
}

func TestOpenInitializesEmptyFile(t *testing.T) {
	path := filepath.Join(privateDirectory(t), "empty.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != storage.CurrentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
