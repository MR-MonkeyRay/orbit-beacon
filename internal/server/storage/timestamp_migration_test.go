package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestTimestampMigrationNormalizesLegacyOffsetsForComparableQueries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(privateDirectory(t), "timestamps.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
INSERT INTO users(id, username, password_hash, role, created_at) VALUES ('u','user','hash','viewer','2026-10-26T14:00:00+02:00');
INSERT INTO nodes(id, name, created_at, first_enrolled_at, last_seen) VALUES ('n','node','2026-10-26T14:00:00+02:00','2026-10-26T14:01:00+02:00','2026-10-26T14:02:00+02:00');
INSERT INTO enrollment_tokens(id, token_hash, node_id, expires_at, revoked_at) VALUES ('t',x'01','n','2026-10-26T14:03:00+02:00','2026-10-26T14:04:00+02:00');
INSERT INTO enrollment_claims(enrollment_id, installation_id, node_id, token_hash, claimed_at) VALUES ('t','installation','n',x'02','2026-10-26T14:05:00+02:00');
INSERT INTO update_jobs(id, release_seq, created_at) VALUES ('j',1,'2026-10-26T14:06:00+02:00');
INSERT INTO audit_events(id, action, created_at) VALUES ('e','legacy','2026-10-26T14:07:00+02:00');
DELETE FROM schema_migrations WHERE version=?`, storage.CurrentSchemaVersion)
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
	checks := []struct {
		query string
		want  string
	}{
		{"SELECT created_at FROM users WHERE id='u'", "2026-10-26T12:00:00.000000000Z"},
		{"SELECT created_at FROM nodes WHERE id='n'", "2026-10-26T12:00:00.000000000Z"},
		{"SELECT first_enrolled_at FROM nodes WHERE id='n'", "2026-10-26T12:01:00.000000000Z"},
		{"SELECT last_seen FROM nodes WHERE id='n'", "2026-10-26T12:02:00.000000000Z"},
		{"SELECT expires_at FROM enrollment_tokens WHERE id='t'", "2026-10-26T12:03:00.000000000Z"},
		{"SELECT revoked_at FROM enrollment_tokens WHERE id='t'", "2026-10-26T12:04:00.000000000Z"},
		{"SELECT claimed_at FROM enrollment_claims WHERE enrollment_id='t'", "2026-10-26T12:05:00.000000000Z"},
		{"SELECT created_at FROM update_jobs WHERE id='j'", "2026-10-26T12:06:00.000000000Z"},
		{"SELECT created_at FROM audit_events WHERE id='e'", "2026-10-26T12:07:00.000000000Z"},
	}
	for _, check := range checks {
		var got string
		if err := db.QueryRowContext(ctx, check.query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", check.query, err)
		}
		if got != check.want {
			t.Errorf("%s = %q, want %q", check.query, got, check.want)
		}
	}
}
