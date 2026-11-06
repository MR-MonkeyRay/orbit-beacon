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

func TestAPITokenLifecycleHashAuditAndUsage(t *testing.T) {
	s, db, now := newAuth(t)
	ctx := context.Background()
	user, err := s.CreateUser(ctx, "api-user", initialPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Login(ctx, user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	days := 30
	metadata, token, err := s.CreateAPIToken(ctx, user.ID, session.Token, "scripts", &days, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 47 || !strings.HasPrefix(token, "obt_") || metadata.LastUsedAt != nil {
		t.Fatal("token shape")
	}
	var stored []byte
	if err = db.QueryRow(`SELECT token_hash FROM api_tokens WHERE id=?`, metadata.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(token))
	if string(stored) != string(digest[:]) {
		t.Fatal("hash differs")
	}
	if _, err = db.Exec(`CREATE TABLE token_writes(n INTEGER); INSERT INTO token_writes VALUES(0); CREATE TRIGGER token_usage AFTER UPDATE OF last_used_at ON api_tokens BEGIN UPDATE token_writes SET n=n+1; END;`); err != nil {
		t.Fatal(err)
	}
	for _, elapsed := range []time.Duration{0, time.Minute, 4*time.Minute + 59*time.Second, 5 * time.Minute, 5*time.Minute + time.Second} {
		*now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(elapsed)
		if err = s.AuthenticateAPIToken(ctx, token); err != nil {
			t.Fatal(err)
		}
		s.WaitAPITokenUse()
	}
	var writes int
	db.QueryRow(`SELECT n FROM token_writes`).Scan(&writes)
	if writes != 2 {
		t.Fatalf("usage writes=%d", writes)
	}
	list, err := s.ListAPITokens(ctx)
	if err != nil || len(list) != 1 || list[0].CreatedByUsername != user.Username || list[0].LastUsedAt == nil {
		t.Fatal("list", err)
	}
	var details string
	if err = db.QueryRow(`SELECT details FROM audit_events WHERE action='api_token.create'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if details != fmt.Sprintf(`{"id":%q,"name":"scripts"}`, metadata.ID) {
		t.Fatalf("audit=%s", details)
	}
	if strings.Contains(details, token) {
		t.Fatal("audit plaintext")
	}
	expiry, err := timekey.Parse(*metadata.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	*now = expiry.Add(-time.Nanosecond)
	if err = s.AuthenticateAPIToken(ctx, token); err != nil {
		t.Fatal("rejected before expiry", err)
	}
	*now = expiry
	if !errors.Is(s.AuthenticateAPIToken(ctx, token), auth.ErrUnauthenticated) {
		t.Fatal("expired accepted")
	}
	list, err = s.ListAPITokens(ctx)
	if err != nil || len(list) != 1 {
		t.Fatal("expired removed")
	}
	if err = s.RevokeAPIToken(audit.WithActor(ctx, user.ID), metadata.ID); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.AuthenticateAPIToken(ctx, token), auth.ErrUnauthenticated) {
		t.Fatal("revoked accepted")
	}
	if err = db.QueryRow(`SELECT details FROM audit_events WHERE action='api_token.revoke'`).Scan(&details); err != nil || details != fmt.Sprintf(`{"id":%q,"name":"scripts"}`, metadata.ID) {
		t.Fatal("revoke audit", details, err)
	}
}

func TestAPITokenCreationRechecksSessionPasswordAndLimit(t *testing.T) {
	s, db, now := newAuth(t)
	ctx := context.Background()
	user, err := s.CreateUser(ctx, "operator", initialPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Login(ctx, user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateAPIToken(ctx, user.ID, session.Token, "test", nil, "wrong"); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatal(err)
	}
	if err = s.Logout(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateAPIToken(ctx, user.ID, session.Token, "test", nil, initialPassword); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked initiating session", err)
	}
	session, err = s.Login(ctx, user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		hash := sha256.Sum256([]byte(fmt.Sprintf("seed-%d", i)))
		if _, err = db.Exec(`INSERT INTO api_tokens(id,name,token_hash,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?)`, fmt.Sprint(i), "expired", hash[:], user.ID, timekey.Format(*now), timekey.Format(now.Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = s.CreateAPIToken(ctx, user.ID, session.Token, "overflow", nil, initialPassword); !errors.Is(err, auth.ErrAPITokenLimit) {
		t.Fatal("limit", err)
	}
	for _, days := range []int{0, 29, 31, 366} {
		if _, _, err = s.CreateAPIToken(ctx, user.ID, session.Token, "invalid", &days, initialPassword); !errors.Is(err, auth.ErrInvalidInput) {
			t.Fatal(err)
		}
	}
}

func TestAPITokenRevokedByAccountRecovery(t *testing.T) {
	for _, operation := range []string{"password", "online-totp", "offline-totp", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, db, now, _, actor, session := totpFixture(t)
			ctx := context.Background()
			user, err := s.CreateUser(ctx, "target", initialPassword, false)
			if err != nil {
				t.Fatal(err)
			}
			targetSession, err := s.Login(ctx, user.Username, initialPassword, "")
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := s.CreateAPIToken(ctx, user.ID, targetSession.Token, "recovery", nil, initialPassword)
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "password":
				err = s.ChangePassword(ctx, user.ID, initialPassword, "replacement password")
			case "online-totp":
				err = s.ResetUserTOTP(ctx, actor.ID, session.Token, user.ID, initialPassword)
			case "offline-totp":
				err = auth.ResetTOTP(ctx, db, user.Username, *now)
			case "delete":
				err = s.DeleteUser(ctx, user.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !errors.Is(s.AuthenticateAPIToken(ctx, token), auth.ErrUnauthenticated) {
				t.Fatal("token survived", operation)
			}
			var count int
			db.QueryRow(`SELECT count(*) FROM api_tokens WHERE created_by=?`, user.ID).Scan(&count)
			if count != 0 {
				t.Fatal("token row survived")
			}
		})
	}
}

func TestAPITokenAuthenticationDoesNotWaitForWriteLock(t *testing.T) {
	s, db, _ := newAuth(t)
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, "usage-lock", initialPassword, false)
	session, _ := s.Login(ctx, user.Username, initialPassword, "")
	metadata, token, err := s.CreateAPIToken(ctx, user.ID, session.Token, "read", nil, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = s.AuthenticateAPIToken(ctx, token)
	elapsed := time.Since(started)
	if _, rollback := conn.ExecContext(ctx, `ROLLBACK`); rollback != nil {
		t.Fatal(rollback)
	}
	if err != nil || elapsed > time.Second {
		t.Fatalf("authentication took %s: %v", elapsed, err)
	}
	s.WaitAPITokenUse()
	var used int
	if err = db.QueryRow(`SELECT count(last_used_at) FROM api_tokens WHERE id=?`, metadata.ID).Scan(&used); err != nil || used != 1 {
		t.Fatal("usage not recorded after the lock cleared", used, err)
	}
}

func TestAPITokenPasswordChangeRequiredAndUsageFailure(t *testing.T) {
	s, db, _ := newAuth(t)
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, "restricted-token", initialPassword, false)
	session, _ := s.Login(ctx, user.Username, initialPassword, "")
	_, token, err := s.CreateAPIToken(ctx, user.ID, session.Token, "read", nil, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER usage_failure BEFORE UPDATE OF last_used_at ON api_tokens BEGIN SELECT RAISE(FAIL,'usage blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.AuthenticateAPIToken(ctx, token); err != nil {
		t.Fatal("usage failure affected authentication", err)
	}
	s.WaitAPITokenUse()
	if _, err = db.Exec(`UPDATE users SET must_change_password=1 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.AuthenticateAPIToken(ctx, token), auth.ErrPasswordChangeRequired) {
		t.Fatal("restricted accepted")
	}
}

func TestAuthConstructorDoesNotReadClock(t *testing.T) {
	auth.NewService(nil, auth.Options{Now: func() time.Time { t.Fatal("constructor read clock"); return time.Time{} }})
}

func TestAPITokenCreationRejectsConcurrentSessionOrPasswordChanges(t *testing.T) {
	for _, change := range []string{"session", "password"} {
		t.Run(change, func(t *testing.T) {
			service, db, now := newAuth(t)
			ctx := context.Background()
			user, err := service.CreateUser(ctx, "concurrent", initialPassword, false)
			if err != nil {
				t.Fatal(err)
			}
			session, err := service.Login(ctx, user.Username, initialPassword, "")
			if err != nil {
				t.Fatal(err)
			}
			random := newBlockingReader(93)
			service = auth.NewService(db, auth.Options{Now: func() time.Time { return *now }, Random: random})
			result := make(chan error, 1)
			go func() {
				_, _, err := service.CreateAPIToken(ctx, user.ID, session.Token, "race", nil, initialPassword)
				result <- err
			}()
			<-random.entered
			if change == "session" {
				_, err = db.Exec(`DELETE FROM sessions`)
			} else {
				hash, e := auth.HashPassword("concurrent replacement password", nil)
				if e != nil {
					close(random.release)
					t.Fatal(e)
				}
				_, err = db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, user.ID)
			}
			close(random.release)
			if err != nil {
				t.Fatal(err)
			}
			expected := auth.ErrUnauthenticated
			if change == "password" {
				expected = auth.ErrWrongPassword
			}
			if err = <-result; !errors.Is(err, expected) {
				t.Fatal("concurrent change accepted", err)
			}
			var count int
			if err = db.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial create", err)
			}
		})
	}
}
