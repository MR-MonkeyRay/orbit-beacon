package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManagementCredentialCleanupPreservesAccountsAndNodes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "upgrade.db")
	migrations := append(probeMigrationsThroughThirteen(), Migration{Version: 14, SQL: probesSchema, Apply: rebuildProbeAlertRules, DisableForeignKeys: true})
	// Reconstruct the removed schema only to exercise upgrades from an existing database.
	migrations[11].SQL = `
CREATE TABLE api_tokens (
 id TEXT PRIMARY KEY, name TEXT NOT NULL CHECK(length(CAST(name AS BLOB)) BETWEEN 1 AND 100),
 token_hash BLOB NOT NULL UNIQUE CHECK(length(token_hash)=32),
 prefix TEXT NOT NULL CHECK(length(prefix)=8),
 created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 created_at TEXT NOT NULL, expires_at TEXT, last_used_at TEXT, revoked_at TEXT
);
CREATE INDEX api_tokens_creator ON api_tokens(created_by);
CREATE INDEX api_tokens_revoked ON api_tokens(revoked_at,id) WHERE revoked_at IS NOT NULL;
CREATE INDEX api_tokens_expired ON api_tokens(expires_at,id) WHERE expires_at IS NOT NULL;`
	db, err := OpenWithMigrations(ctx, path, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO users(id,username,password_hash,role,created_at) VALUES('owner','admin','password-hash','admin','2026-10-27T00:00:00.000000000Z');
INSERT INTO nodes(id,name,created_at) VALUES('node','retained node','2026-10-27T00:00:00.000000000Z');
INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(zeroblob(32),'owner','2026-10-28T00:00:00.000000000Z');
INSERT INTO api_tokens(id,name,token_hash,prefix,created_by,created_at) VALUES('token','monitor',zeroblob(32),'token-id','owner','2026-10-27T00:00:00.000000000Z');`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var obsolete int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='api_tokens' OR tbl_name='api_tokens'`).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatalf("obsolete credential schema objects=%d err=%v", obsolete, err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM users WHERE id='owner' AND username='admin' AND password_hash='password-hash' AND role='admin'`: 1,
		`SELECT count(*) FROM nodes WHERE id='node' AND name='retained node'`:                                                 1,
		`SELECT count(*) FROM sessions WHERE user_id='owner' AND token_hash=zeroblob(32)`:                                     1,
	} {
		var got int
		if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got=%d want=%d err=%v", query, got, want, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var nodes int
	if err := db.QueryRow(`SELECT count(*) FROM nodes WHERE id='node'`).Scan(&nodes); err != nil || nodes != 1 {
		t.Fatalf("reopened nodes=%d err=%v", nodes, err)
	}
}
