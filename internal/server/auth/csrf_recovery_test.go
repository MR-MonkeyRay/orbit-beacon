package auth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
)

func TestLegacySessionSurvivesCSRFRecoveryMigration(t *testing.T) {
	service, db, now := newAuth(t)
	ctx := context.Background()
	if _, err := service.BootstrapAdministrator(ctx, "admin", initialPassword); err != nil {
		t.Fatal(err)
	}
	active, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the version-5 session representation with an unrecoverable hash.
	if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN csrf_hash BLOB NOT NULL DEFAULT x'00'; DELETE FROM schema_migrations WHERE version=6"); err != nil {
		t.Fatal(err)
	}
	legacyCSRF := strings.Repeat("A", 43)
	legacyHash := sha256.Sum256([]byte(legacyCSRF))
	if _, err := db.Exec("UPDATE sessions SET csrf_hash=?", legacyHash[:]); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	restarted := auth.NewService(migrated, auth.Options{Now: func() time.Time { return *now }})
	recovered, err := restarted.CurrentSession(ctx, active.Token)
	if err != nil || recovered.User.ID != active.User.ID || !recovered.ExpiresAt.Equal(active.ExpiresAt) || recovered.Token != "" {
		t.Fatalf("migration invalidated or exposed active cookie: %+v err=%v", recovered, err)
	}
	if valid, err := restarted.VerifyCSRF(ctx, active.Token, recovered.CSRFToken); err != nil || !valid {
		t.Fatalf("migrated session cannot mutate: valid=%v err=%v", valid, err)
	}
	if valid, err := restarted.VerifyCSRF(ctx, active.Token, legacyCSRF); err != nil || valid {
		t.Fatalf("obsolete CSRF hash remains accepted: valid=%v err=%v", valid, err)
	}
	*now = active.ExpiresAt
	if _, err := restarted.CurrentSession(ctx, active.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("recovery revived an expired session: %v", err)
	}
	if valid, err := restarted.VerifyCSRF(ctx, active.Token, recovered.CSRFToken); err != nil || valid {
		t.Fatalf("expired recovered token accepted: valid=%v err=%v", valid, err)
	}
}
