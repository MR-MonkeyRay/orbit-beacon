package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestEnrollmentClaimsSurviveNodeDeletionAndCascadeWithToken(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(privateDirectory(t), "enrollment.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
INSERT INTO nodes(id,name,created_at) VALUES('n','existing','2026-10-26T12:00:00Z');
INSERT INTO enrollment_tokens(id,token_hash,expires_at) VALUES('t',x'01','2026-10-26T12:30:00Z');
INSERT INTO enrollment_claims(enrollment_id,installation_id,node_id,token_hash,claimed_at) VALUES('t','installation','n',x'02','2026-10-26T12:00:00Z');`)
	if err != nil {
		_ = db.Close()
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
