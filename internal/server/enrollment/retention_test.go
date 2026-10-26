package enrollment

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestCleanupExpiredTicketsIsBoundedAndCascadesReceipts(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO nodes(id, name, created_at) VALUES ('node', 'node', ?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for _, ticket := range []struct{ id, expires string }{{"old-a", old}, {"old-b", old}, {"recent", recent}} {
		if _, err := db.Exec(`INSERT INTO enrollment_tokens(id, token_hash, node_id, expires_at) VALUES (?, ?, 'node', ?)`, ticket.id, []byte("hash-"+ticket.id), ticket.expires); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO enrollment_claims(enrollment_id, installation_id, node_id, token_hash, claimed_at) VALUES (?, ?, 'node', ?, ?)`, ticket.id, "installation-"+ticket.id, []byte("claim-"+ticket.id), ticket.expires); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := NewService(db, Options{}).CleanupExpiredTickets(context.Background(), time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), 1)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want one bounded deletion", removed)
	}
	var oldTickets, oldReceipts, recentTickets, recentReceipts int
	if err := db.QueryRow(`SELECT count(*) FROM enrollment_tokens WHERE expires_at < ?`, recent).Scan(&oldTickets); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM enrollment_claims WHERE enrollment_id IN ('old-a', 'old-b')`).Scan(&oldReceipts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM enrollment_tokens WHERE id='recent'`).Scan(&recentTickets); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM enrollment_claims WHERE enrollment_id='recent'`).Scan(&recentReceipts); err != nil {
		t.Fatal(err)
	}
	if oldTickets != 1 || oldReceipts != 1 || recentTickets != 1 || recentReceipts != 1 {
		t.Fatalf("bounded cleanup state: old tickets=%d old receipts=%d recent tickets=%d recent receipts=%d", oldTickets, oldReceipts, recentTickets, recentReceipts)
	}
	if _, err := NewService(db, Options{}).CleanupExpiredTickets(context.Background(), time.Now(), 0); err != ErrInvalidInput {
		t.Fatalf("limit 0 error=%v, want ErrInvalidInput", err)
	}
}
