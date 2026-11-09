package storage

import (
	"fmt"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestMaintenanceVacuumFullKeepsActiveHAWritable(t *testing.T) {
	source, _ := replicaPair(t, MainDomain)
	source.current.writer.SetMaxOpenConns(1)
	source.current.writer.SetMaxIdleConns(1)
	lease, err := source.Acquire(t.Context(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for key, value := range map[string]string{"preserved": "before", "reserve": strings.Repeat("q", 64<<10)} {
		if _, err = tx.ExecContext(lease.Context(), "INSERT INTO settings VALUES(?,?)", key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	replicaWrite(t, source, "DELETE FROM settings WHERE `key`='reserve'")
	var pages, maximum int
	if err = source.current.writer.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err = source.current.writer.QueryRowContext(t.Context(), fmt.Sprintf("PRAGMA max_page_count=%d", pages)).Scan(&maximum); err != nil || maximum != pages {
		t.Fatalf("page quota=%d want=%d err=%v", maximum, pages, err)
	}
	// Obtain a real engine FULL without routing this fixture's DML through lease.failure.
	raw, err := source.current.writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Rollback()
	if _, err = raw.ExecContext(t.Context(), "UPDATE replica_state SET capture_mode='apply' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	_, full := raw.ExecContext(t.Context(), "INSERT INTO settings VALUES('too-large',?)", strings.Repeat("x", 1<<20))
	if code, ok := SQLiteCode(full); !ok || code != sqlite3.SQLITE_FULL {
		t.Fatalf("expected real SQLITE_FULL: code=%d err=%v", code, full)
	}
	_ = raw.Rollback()
	var value, mode string
	var oversized int
	if err = lease.QueryRowContext(lease.Context(), "SELECT value FROM settings WHERE `key`='preserved'").Scan(&value); err != nil || value != "before" {
		t.Fatalf("FULL lost original data: %q %v", value, err)
	}
	if err = lease.QueryRowContext(lease.Context(), "SELECT capture_mode,(SELECT COUNT(*) FROM settings WHERE `key`='too-large') FROM replica_state WHERE id=1").Scan(&mode, &oversized); err != nil || mode != "source" || oversized != 0 {
		t.Fatalf("FULL did not roll back: mode=%s oversized=%d err=%v", mode, oversized, err)
	}
	generation := source.Generation()
	// This tests VACUUM-only error routing, not a temporary-space fault inside VACUUM itself.
	failure := lease.vacuumFailure(full)
	if code, ok := SQLiteCode(failure); !ok || code != sqlite3.SQLITE_FULL {
		t.Fatalf("compaction error lost its safe numeric cause: %v", failure)
	}
	if !IsConnectionFailure(full) {
		t.Fatal("ordinary SQLITE_FULL stopped being a storage failure")
	}
	if !source.Ready() || source.Generation() != generation || lease.Context().Err() != nil {
		t.Fatal("VACUUM-only FULL froze the active HA generation")
	}
	tx, err = lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(lease.Context(), "UPDATE settings SET value='after' WHERE `key`='preserved'"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = lease.QueryRowContext(lease.Context(), "SELECT value FROM settings WHERE `key`='preserved'").Scan(&value); err != nil || value != "after" {
		t.Fatalf("normal reuse write failed after FULL: %q %v", value, err)
	}
}
