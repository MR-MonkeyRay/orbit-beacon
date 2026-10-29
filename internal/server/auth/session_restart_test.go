package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
)

func TestSessionSurvivesDatabaseRestart(t *testing.T) {
	service, db, now := newAuth(t)
	ctx := context.Background()
	if _, err := service.BootstrapAdministrator(ctx, "admin", initialPassword); err != nil {
		t.Fatal(err)
	}
	active, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	if err := db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := auth.NewService(reopened, auth.Options{Now: func() time.Time { return *now }})
	recovered, err := restarted.CurrentSession(ctx, active.Token)
	if err != nil || recovered.User.ID != active.User.ID || recovered.User.Username != active.User.Username || !recovered.User.MustChangePassword || !recovered.ExpiresAt.Equal(active.ExpiresAt) || recovered.Token != "" || recovered.CSRFToken != active.CSRFToken {
		t.Fatalf("restart changed or exposed active session: %+v err=%v", recovered, err)
	}
	if _, err := restarted.Authenticate(ctx, active.Token); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatalf("restart removed initial password restriction: %v", err)
	}
	if valid, err := restarted.VerifyCSRF(ctx, active.Token, recovered.CSRFToken); err != nil || !valid {
		t.Fatalf("restarted session cannot mutate: valid=%v err=%v", valid, err)
	}
	if valid, err := restarted.VerifyCSRF(ctx, active.Token, active.Token); err != nil || valid {
		t.Fatalf("random cookie accepted as CSRF token: valid=%v err=%v", valid, err)
	}
	*now = active.ExpiresAt
	if _, err := restarted.CurrentSession(ctx, active.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("restart revived an expired session: %v", err)
	}
	if valid, err := restarted.VerifyCSRF(ctx, active.Token, recovered.CSRFToken); err != nil || valid {
		t.Fatalf("expired recovered token accepted: valid=%v err=%v", valid, err)
	}
}
