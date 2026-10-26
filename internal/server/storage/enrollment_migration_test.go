package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestExistingClaimsSurviveMigrationAndAssetDeletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(privateDirectory(t), "migration.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the installed v3 claim table with an existing successful receipt.
	_, err = db.Exec(`
DROP TABLE enrollment_claims;
CREATE TABLE enrollment_claims (
 enrollment_id TEXT NOT NULL REFERENCES enrollment_tokens(id) ON DELETE CASCADE,
 installation_id TEXT NOT NULL, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 token_hash BLOB NOT NULL, claimed_at TEXT NOT NULL, PRIMARY KEY(enrollment_id,installation_id)
);
DELETE FROM schema_migrations WHERE version = 4;
INSERT INTO nodes(id,name,created_at) VALUES('n','existing','2026-10-26T12:00:00Z');
INSERT INTO enrollment_tokens(id,token_hash,expires_at) VALUES('t',x'01','2026-10-26T12:30:00Z');
INSERT INTO enrollment_claims VALUES('t','installation','n',x'02','2026-10-26T12:00:00Z');`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM nodes WHERE id='n'"); err != nil {
		t.Fatal(err)
	}
	var node, installation string
	var hash []byte
	if err := db.QueryRow("SELECT node_id,installation_id,token_hash FROM enrollment_claims WHERE enrollment_id='t'").Scan(&node, &installation, &hash); err != nil {
		t.Fatalf("existing receipt was lost: %v", err)
	}
	if node != "n" || installation != "installation" || len(hash) != 1 || hash[0] != 2 {
		t.Fatalf("receipt corrupted: node=%q installation=%q hash=%x", node, installation, hash)
	}
	if _, err := db.Exec("DELETE FROM enrollment_tokens WHERE id='t'"); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRow("SELECT count(*) FROM enrollment_claims").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("ticket deletion did not reclaim receipts: remaining=%d err=%v", remaining, err)
	}
}
