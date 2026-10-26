package storage_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestBusyClassificationUsesWrappedSQLiteCodesNotMessages(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(privateDirectory(t), "errors.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "CREATE TABLE contention_probe(value INTEGER UNIQUE); INSERT INTO contention_probe VALUES(1),(2)"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(context.Background(), "SELECT value FROM contention_probe")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("contention fixture has no active row")
	}
	_, locked := conn.ExecContext(context.Background(), "DROP TABLE contention_probe")
	rows.Close()
	if locked == nil || !storage.IsBusy(fmt.Errorf("transaction: %w", locked)) {
		t.Fatalf("SQLite LOCKED was not retryable: %v", locked)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN DEFERRED"); err != nil {
		t.Fatal(err)
	}
	var value int
	if err := conn.QueryRowContext(context.Background(), "SELECT value FROM contention_probe WHERE value=1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE contention_probe SET value=3 WHERE value=2"); err != nil {
		t.Fatal(err)
	}
	_, snapshot := conn.ExecContext(context.Background(), "UPDATE contention_probe SET value=4 WHERE value=1")
	var coded interface{ Code() int }
	if !errors.As(snapshot, &coded) || coded.Code() != sqlite3.SQLITE_BUSY_SNAPSHOT || !storage.IsBusy(fmt.Errorf("snapshot: %w", snapshot)) {
		t.Fatalf("extended SQLite BUSY_SNAPSHOT was not retryable: %v", snapshot)
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"SELECT * FROM missing_table", "INSERT INTO contention_probe VALUES(1)"} {
		_, failure := conn.ExecContext(context.Background(), query)
		if failure == nil || storage.IsBusy(failure) {
			t.Fatalf("noncontention SQLite failure masked as retryable: %v", failure)
		}
	}
	if storage.IsBusy(nil) || storage.IsBusy(errors.New("database is locked")) {
		t.Fatal("classification accepted a message rather than a SQLite code")
	}
}
