package auth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/timekey"
)

func tokenOwner(t *testing.T, s *auth.Service) auth.User {
	t.Helper()
	owner, err := s.BootstrapAdministrator(context.Background(), "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestAPITokenLifecycleHashAuditAndUseThrottle(t *testing.T) {
	s, db, now := newAuth(t)
	owner := tokenOwner(t, s)
	ctx := audit.WithActor(context.Background(), owner.ID)
	days := 1
	created, err := s.CreateAPIToken(ctx, owner.ID, " monitoring ", &days)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Token) != 47 || !strings.HasPrefix(created.Token, "obt_") || created.Name != "monitoring" || created.Prefix != created.ID[:8] || created.ExpiresAt == nil {
		t.Fatalf("bad created metadata: %+v", created.APIToken)
	}
	var hash []byte
	if err = db.QueryRow(`SELECT token_hash FROM api_tokens WHERE id=?`, created.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(created.Token))
	if string(hash) != string(digest[:]) || strings.Contains(string(hash), created.Token) {
		t.Fatal("token not stored as SHA-256")
	}
	if _, err = s.AuthenticateAPIToken(ctx, created.Token); err != nil {
		t.Fatal(err)
	}
	s.TouchAPIToken(ctx, created.ID)
	var used string
	if err = db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id=?`, created.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != timekey.Format(*now) {
		t.Fatalf("last_used=%q", used)
	}
	first := used
	*now = now.Add(59 * time.Second)
	s.TouchAPIToken(ctx, created.ID)
	db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id=?`, created.ID).Scan(&used)
	if used != first {
		t.Fatal("timestamp churn within one minute")
	}
	*now = now.Add(time.Second)
	s.TouchAPIToken(ctx, created.ID)
	db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id=?`, created.ID).Scan(&used)
	if used != timekey.Format(*now) {
		t.Fatal("timestamp not updated at minute boundary")
	}
	if err = s.RevokeAPIToken(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateAPIToken(ctx, created.Token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("revoked: %v", err)
	}
	if err = s.RevokeAPIToken(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"api_token.create", "api_token.revoke"} {
		var count int
		var details string
		if err = db.QueryRow(`SELECT count(*),details FROM audit_events WHERE target_id=? AND action=?`, created.ID, action).Scan(&count, &details); err != nil {
			t.Fatal(err)
		}
		if count != 1 || strings.Contains(details, created.Token) || details != "{}" {
			t.Fatalf("audit %s count=%d details=%s", action, count, details)
		}
	}
	tokens, err := s.ListAPITokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].RevokedAt == nil {
		t.Fatalf("list: %+v %v", tokens, err)
	}
}

func TestAPITokenExpiryCapacityValidationAndCascade(t *testing.T) {
	s, db, now := newAuth(t)
	owner := tokenOwner(t, s)
	ctx := context.Background()
	for _, days := range []int{0, -1, 366} {
		if _, err := s.CreateAPIToken(ctx, owner.ID, "valid", &days); !errors.Is(err, auth.ErrInvalidInput) {
			t.Fatalf("days %d: %v", days, err)
		}
	}
	for _, name := range []string{"", "  ", strings.Repeat("x", 101), "bad\x00name", string([]byte{0xff})} {
		if _, err := s.CreateAPIToken(ctx, owner.ID, name, nil); !errors.Is(err, auth.ErrInvalidInput) {
			t.Fatalf("bad name: %v", err)
		}
	}
	day := 1
	expiring, err := s.CreateAPIToken(ctx, owner.ID, "expires", &day)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(24 * time.Hour)
	if _, err = s.AuthenticateAPIToken(ctx, expiring.Token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expired: %v", err)
	}
	var first auth.CreatedAPIToken
	for i := range 50 {
		created, err := s.CreateAPIToken(ctx, owner.ID, fmt.Sprintf("token %d", i), nil)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if i == 0 {
			first = created
		}
	}
	if _, err = s.CreateAPIToken(ctx, owner.ID, "over capacity", nil); !errors.Is(err, auth.ErrTokenCapacity) {
		t.Fatalf("cap: %v", err)
	}
	if err = s.RevokeAPIToken(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateAPIToken(ctx, owner.ID, "replacement", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DELETE FROM users WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&count)
	if count != 0 {
		t.Fatal("creator deletion did not cascade")
	}
}

func TestAPITokenRateLimits(t *testing.T) {
	s, _, now := newAuth(t)
	owner := tokenOwner(t, s)
	ctx := context.Background()
	first, err := s.CreateAPIToken(ctx, owner.ID, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateAPIToken(ctx, owner.ID, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.CreateAPIToken(ctx, owner.ID, "third", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		if _, err = s.AuthenticateAPIToken(ctx, first.Token); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.AuthenticateAPIToken(ctx, first.Token); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("token limit: %v", err)
	}
	if _, err = s.AuthenticateAPIToken(ctx, second.Token); err != nil {
		t.Fatalf("second shares budget: %v", err)
	}
	*now = now.Add(100 * time.Millisecond)
	if _, err = s.AuthenticateAPIToken(ctx, first.Token); err != nil {
		t.Fatalf("refill: %v", err)
	}
	peer := auth.WithLoginPeer(ctx, "192.0.2.1:1234")
	for range 30 {
		if _, err = s.AuthenticateAPIToken(peer, "obt_invalid"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatal(err)
		}
	}
	if _, err = s.AuthenticateAPIToken(peer, "obt_invalid"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("peer limit: %v", err)
	}
	if _, err = s.AuthenticateAPIToken(peer, second.Token); err != nil {
		t.Fatalf("invalid budget blocked a valid token: %v", err)
	}
	// A valid token that never authenticated before is still accepted.
	if _, err = s.AuthenticateAPIToken(peer, third.Token); err != nil {
		t.Fatalf("invalid budget blocked an unseen valid token: %v", err)
	}
	if _, err = s.AuthenticateAPIToken(auth.WithLoginPeer(ctx, "192.0.2.2:1"), "obt_invalid"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("other peer: %v", err)
	}
	*now = now.Add(time.Second)
	if _, err = s.AuthenticateAPIToken(peer, "obt_invalid"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("peer refill: %v", err)
	}
}

func TestAPITokenAuditFailureRollsBack(t *testing.T) {
	s, db, _ := newAuth(t)
	owner := tokenOwner(t, s)
	ctx := audit.WithActor(context.Background(), owner.ID)
	token, err := s.CreateAPIToken(ctx, owner.ID, "existing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER deny_token_audit BEFORE INSERT ON audit_events WHEN NEW.action LIKE 'api_token.%' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateAPIToken(ctx, owner.ID, "rollback", nil); err == nil {
		t.Fatal("creation ignored audit failure")
	}
	if err = s.RevokeAPIToken(ctx, token.ID); err == nil {
		t.Fatal("revocation ignored audit failure")
	}
	if _, err = s.AuthenticateAPIToken(ctx, token.Token); err != nil {
		t.Fatalf("failed revoke took effect: %v", err)
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&count)
	if count != 1 {
		t.Fatal("failed creation committed")
	}
}

func TestAPITokenTouchSkipsBusyWriter(t *testing.T) {
	s, db, _ := newAuth(t)
	owner := tokenOwner(t, s)
	ctx := context.Background()
	token, err := s.CreateAPIToken(ctx, owner.ID, "busy", nil)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	start := time.Now()
	s.TouchAPIToken(ctx, token.ID)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("last-used update waited for busy writer")
	}
	var used any
	if err = db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id=?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != nil {
		t.Fatal("busy update unexpectedly persisted")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Every pooled connection must retain its standard timeout.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var timeout int
	if err = conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("timeout=%d err=%v", timeout, err)
	}
	s.TouchAPIToken(ctx, token.ID)
	if err = db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id=?`, token.ID).Scan(&used); err != nil || used == nil {
		t.Fatalf("retry did not persist: %v", err)
	}
}

func TestAPITokenRetentionBoundariesAndBatch(t *testing.T) {
	s, db, now := newAuth(t)
	owner := tokenOwner(t, s)
	ctx := context.Background()
	cutoff := now.Add(-30 * 24 * time.Hour)
	old := timekey.Format(cutoff.Add(-time.Second))
	boundary := timekey.Format(cutoff)
	for i := range 6 {
		token, err := s.CreateAPIToken(ctx, owner.ID, fmt.Sprint(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		var revoked, expiry any
		switch i {
		case 0, 1:
			revoked = old
		case 2:
			expiry = old
		case 3:
			revoked = boundary
		case 4:
			expiry = boundary
		}
		if _, err = db.Exec(`UPDATE api_tokens SET revoked_at=?,expires_at=? WHERE id=?`, revoked, expiry, token.ID); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.CleanupAPITokens(ctx, cutoff, 2)
	if err != nil || removed != 2 {
		t.Fatalf("batch %d %v", removed, err)
	}
	removed, err = s.CleanupAPITokens(ctx, cutoff, 2)
	if err != nil || removed != 1 {
		t.Fatalf("rest %d %v", removed, err)
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&count)
	if count != 3 {
		t.Fatalf("retention removed recent/active tokens: %d", count)
	}
}

func TestAPITokenConcurrentCreationKeepsGlobalCap(t *testing.T) {
	s, _, _ := newAuth(t)
	owner := tokenOwner(t, s)
	ctx := context.Background()
	for i := range 49 {
		if _, err := s.CreateAPIToken(ctx, owner.ID, fmt.Sprint(i), nil); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := s.CreateAPIToken(ctx, owner.ID, "concurrent", nil); results <- err }()
	}
	successes, limited := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, auth.ErrTokenCapacity):
			limited++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || limited != 1 {
		t.Fatalf("concurrent cap successes=%d limited=%d", successes, limited)
	}
}
