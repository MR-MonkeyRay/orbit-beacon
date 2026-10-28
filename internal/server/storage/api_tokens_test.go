package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAPITokenMigrationFromEleven(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "v11.db")
	ctx := context.Background()
	migrations := []Migration{{Version: 1, SQL: foundationSchema}, {Version: 2, SQL: sessionsExpirySchema}, {Version: 3, SQL: reportIdentitySchema}, {Version: 4, SQL: enrollmentTombstoneSchema}, {Version: 5, Apply: repairReportSelections}, {Version: 6, SQL: recoverableCSRFSchema}, {Version: 7, SQL: auditOrderingSchema}, {Version: 8, SQL: enrollmentOrderingSchema}, {Version: 9, Apply: normalizeTimestampColumns}, {Version: 10, SQL: alertsSchema}, {Version: 11, SQL: notificationsSchema}}
	db, err := OpenWithMigrations(ctx, path, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at) VALUES('owner','admin','hash','admin','2026-10-27T00:00:00.000000000Z')`); err != nil {
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
	var version int
	if err = db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 14 || CurrentSchemaVersion != 14 {
		t.Fatalf("schema %d: %v", version, err)
	}
	if _, err = db.Exec(`INSERT INTO api_tokens(id,name,token_hash,prefix,created_by,created_at) VALUES('token','monitor',zeroblob(32),'token-id','owner','2026-10-27T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO api_tokens(id,name,token_hash,prefix,created_by,created_at) VALUES('duplicate','monitor',zeroblob(32),'other-id','owner','now')`); err == nil {
		t.Fatal("duplicate hash accepted")
	}
	if _, err = db.Exec(`INSERT INTO api_tokens(id,name,token_hash,prefix,created_by,created_at) VALUES('orphan','monitor',randomblob(32),'other-id','missing','now')`); err == nil {
		t.Fatal("unknown creator accepted")
	}
	if _, err = db.Exec(`UPDATE api_tokens SET name='' WHERE id='token'`); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err = db.Exec(`DELETE FROM users WHERE id='owner'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade count %d: %v", count, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal("reopening migration 12", err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
