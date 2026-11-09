package auth_test

import (
	"bytes"
	"context"
	"errors"

	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLAccountFactorsTokensAndCleanup(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon account secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewService(db, auth.Options{Now: func() time.Time { return now }, SecretBox: box})
	ctx := context.Background()
	user, err := s.CreateUser(ctx, "mysql-admin", initialPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Login(ctx, user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	metadata, token, err := s.CreateAPIToken(ctx, user.ID, session.Token, "Case trailing ", nil, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGlobalAPITokenRateLimit(ctx, 120); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthenticateAPIToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	s.WaitAPITokenUse()
	tokens, err := s.ListAPITokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].LastUsedAt == nil || tokens[0].Name != "Case trailing " {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
	if err := s.RevokeAPIToken(audit.WithActor(ctx, user.ID), metadata.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthenticateAPIToken(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal(err)
	}
	raw := f.Raw(t, storage.MainDomain)
	if _, err := raw.Exec(`UPDATE users SET totp_ciphertext=? WHERE id=?`, []byte{}, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, user.Username, initialPassword, "123456"); !errors.Is(err, auth.ErrTOTPUnavailable) {
		t.Fatalf("empty BLOB became NULL: %v", err)
	}
	if _, err := raw.Exec(`UPDATE users SET totp_ciphertext=NULL WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	secret, codes := enableTOTP(t, s, user, session, now)
	now = now.Add(30 * time.Second)
	second := auth.NewService(db, auth.Options{Now: func() time.Time { return now }, SecretBox: box})
	for _, code := range []string{testOTP(secret, now), codes[0]} {
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, service := range []*auth.Service{s, second} {
			wg.Add(1)
			go func(service *auth.Service) {
				defer wg.Done()
				_, err := service.Login(ctx, user.Username, initialPassword, code)
				results <- err
			}(service)
		}
		wg.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			} else if !errors.Is(err, auth.ErrTOTPInvalid) {
				t.Fatal(err)
			}
		}
		if successes != 1 {
			t.Fatalf("factor accepted %d times", successes)
		}
	}
	if err := s.Logout(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	if _, err := s.CleanupExpiredSessions(ctx); err != nil {
		t.Fatal(err)
	}
}
