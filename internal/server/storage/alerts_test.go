package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAlertsMigrationFromVersionNine(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "v9.db")
	db, err := OpenWithMigrations(ctx, path, []Migration{{Version: 1, SQL: foundationSchema}, {Version: 2, SQL: sessionsExpirySchema}, {Version: 3, SQL: reportIdentitySchema}, {Version: 4, SQL: enrollmentTombstoneSchema}, {Version: 5, Apply: repairReportSelections}, {Version: 6, SQL: recoverableCSRFSchema}, {Version: 7, SQL: auditOrderingSchema}, {Version: 8, SQL: enrollmentOrderingSchema}, {Version: 9, Apply: normalizeTimestampColumns}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO groups(id,name) VALUES('retained','retained')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err = db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("version=%d %v", version, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM groups WHERE id='retained'").Scan(&count); err != nil || count != 1 {
		t.Fatal("migration lost old data", err)
	}
	for _, table := range []string{"alert_rules", "alert_events"} {
		if err = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if _, err = db.Exec("INSERT INTO alert_rules(id,name,enabled,scope_kind,scope_id,metric,for_seconds,severity,created_at,updated_at) VALUES('r','offline',1,'group','retained','offline',180,'critical','now','now'); DELETE FROM groups WHERE id='retained'"); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM alert_rules").Scan(&count); err != nil || count != 0 {
		t.Fatal("generated scope FK did not cascade", err)
	}
}
