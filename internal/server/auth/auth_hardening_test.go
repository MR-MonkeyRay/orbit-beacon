package auth_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"orbitbeacon/internal/server/auth"
)

type blockingReader struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	source  io.Reader
}

func newBlockingReader(seed byte) *blockingReader {
	var key [32]byte
	key[0] = seed
	return &blockingReader{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		source:  rand.NewChaCha8(key),
	}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.entered)
		<-r.release
	})
	return r.source.Read(p)
}

func TestChangePasswordRateLimitAndWrongPasswordError(t *testing.T) {
	service, db, now := newAuth(t)
	user, err := service.BootstrapAdministrator(context.Background(), "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 5 {
		err := service.ChangePassword(context.Background(), user.ID, fmt.Sprintf("wrong password %d", i), "unused replacement password")
		if !errors.Is(err, auth.ErrWrongPassword) {
			t.Fatalf("wrong-password attempt %d error = %v, want ErrWrongPassword", i+1, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(context.Background(), user.ID, initialPassword, "unused replacement password"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("sixth password change reached the closed database: %v", err)
	}
	*now = now.Add(time.Minute)
	if err := service.ChangePassword(context.Background(), user.ID, initialPassword, "unused replacement password"); errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("password-change budget did not refill after one minute: %v", err)
	}
}

func TestChangePasswordCASConflictPreservesConcurrentPasswordAndSession(t *testing.T) {
	service, db, now := newAuth(t)
	ctx := context.Background()
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	concurrentHash, err := auth.HashPassword("concurrent replacement password", rand.NewChaCha8([32]byte{41}))
	if err != nil {
		t.Fatal(err)
	}

	random := newBlockingReader(42)
	service = auth.NewService(db, auth.Options{Now: func() time.Time { return *now }, Random: random})
	result := make(chan error, 1)
	go func() {
		result <- service.ChangePassword(ctx, user.ID, initialPassword, "requested replacement password")
	}()
	<-random.entered
	if _, err := db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", concurrentHash, user.ID); err != nil {
		close(random.release)
		t.Fatal(err)
	}
	close(random.release)
	if err := <-result; !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatalf("CAS conflict error = %v, want ErrWrongPassword", err)
	}

	var storedHash string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", user.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != concurrentHash {
		t.Fatal("failed CAS overwrote the concurrently changed password")
	}
	if got, err := service.Authenticate(ctx, session.Token); err != nil || got.ID != user.ID {
		t.Fatalf("failed CAS revoked the existing session: user=%+v err=%v", got, err)
	}
	var audits int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM audit_events WHERE action = 'user.password.change' AND target_id = ?", user.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatalf("failed CAS inserted %d password-change audit events", audits)
	}
}

func TestPasswordHashingDoesNotHoldSQLiteWriter(t *testing.T) {
	t.Run("change password", func(t *testing.T) {
		service, db, now := newAuth(t)
		user, err := service.BootstrapAdministrator(context.Background(), "admin", initialPassword)
		if err != nil {
			t.Fatal(err)
		}
		random := newBlockingReader(51)
		service = auth.NewService(db, auth.Options{Now: func() time.Time { return *now }, Random: random})
		assertWriterAvailableDuringEntropy(t, db, random, func() error {
			return service.ChangePassword(context.Background(), user.ID, initialPassword, "replacement password")
		})
	})

	t.Run("create user", func(t *testing.T) {
		_, db, now := newAuth(t)
		random := newBlockingReader(52)
		service := auth.NewService(db, auth.Options{Now: func() time.Time { return *now }, Random: random})
		assertWriterAvailableDuringEntropy(t, db, random, func() error {
			_, err := service.CreateUser(context.Background(), "admin-two", initialPassword)
			return err
		})
	})

	t.Run("bootstrap administrator", func(t *testing.T) {
		_, db, now := newAuth(t)
		random := newBlockingReader(53)
		service := auth.NewService(db, auth.Options{Now: func() time.Time { return *now }, Random: random})
		assertWriterAvailableDuringEntropy(t, db, random, func() error {
			_, err := service.BootstrapAdministrator(context.Background(), "admin", initialPassword)
			return err
		})
	})
}

func assertWriterAvailableDuringEntropy(t *testing.T, db *sql.DB, random *blockingReader, operation func() error) {
	t.Helper()
	operationResult := make(chan error, 1)
	go func() { operationResult <- operation() }()
	<-random.entered

	writeResult := make(chan error, 1)
	go func() {
		_, err := db.Exec("INSERT INTO audit_events(id, action, created_at) VALUES (?, 'writer.probe', ?)", "writer-probe", "2026-01-02T03:04:05.000000000Z")
		writeResult <- err
	}()
	select {
	case err := <-writeResult:
		if err != nil {
			close(random.release)
			t.Fatalf("independent write failed while password hashing was blocked: %v", err)
		}
	case <-time.After(time.Second):
		close(random.release)
		t.Fatal("password hashing held the SQLite writer lock")
	}
	close(random.release)
	if err := <-operationResult; err != nil {
		t.Fatalf("operation failed after entropy resumed: %v", err)
	}
}

func TestLoginRehashesInBoundsNonDefaultArgon2Parameters(t *testing.T) {
	service, db, _ := newAuth(t)
	ctx := auth.WithLoginPeer(context.Background(), "192.0.2.10:1234")
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	legacy := encodedPassword(initialPassword, 19456, 1, 2)
	if _, err := db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", legacy, user.ID); err != nil {
		t.Fatal(err)
	}

	session, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, session.Token); err != nil {
		t.Fatalf("session created during password rehash is invalid: %v", err)
	}
	var current string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", user.ID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current == legacy || !strings.Contains(current, "$m=32768,t=2,p=1$") {
		t.Fatalf("successful login did not replace nondefault hash: %q", current)
	}
	if ok, err := auth.VerifyPassword(initialPassword, current); err != nil || !ok {
		t.Fatalf("replacement hash does not verify: ok=%t err=%v", ok, err)
	}
}

func TestWrongPasswordDoesNotRehashNonDefaultArgon2Parameters(t *testing.T) {
	service, db, _ := newAuth(t)
	ctx := auth.WithLoginPeer(context.Background(), "192.0.2.11:1234")
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	legacy := encodedPassword(initialPassword, 19456, 1, 2)
	if _, err := db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", legacy, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, "admin", "wrong password for test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("wrong-password login error = %v", err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", user.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != legacy {
		t.Fatal("wrong password caused an Argon2 parameter rehash")
	}
}

func TestLoginUsernameFailureBudgetChargesOnlyFailuresAndResetsOnSuccess(t *testing.T) {
	t.Run("known username", func(t *testing.T) {
		service, _, _ := newAuth(t)
		ctx := context.Background()
		if _, err := service.BootstrapAdministrator(ctx, "admin", initialPassword); err != nil {
			t.Fatal(err)
		}
		trusted := auth.WithLoginPeer(ctx, "192.0.2.200:1234")
		if _, err := service.Login(trusted, "admin", initialPassword); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 20; i++ {
			attacker := auth.WithLoginPeer(ctx, fmt.Sprintf("198.51.100.%d:1234", i))
			if _, err := service.Login(attacker, "admin", "wrong password for test"); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("failed verification %d error = %v, want ErrUnauthenticated", i, err)
			}
		}
		newSource := auth.WithLoginPeer(ctx, "203.0.113.1:1234")
		if _, err := service.Login(newSource, "admin", "wrong password for test"); !errors.Is(err, auth.ErrRateLimited) {
			t.Fatalf("21st failed verification error = %v, want ErrRateLimited", err)
		}
		if _, err := service.Login(trusted, "admin", initialPassword); err != nil {
			t.Fatalf("trusted source was locked out by the exhausted username budget: %v", err)
		}
		if _, err := service.Login(newSource, "admin", "wrong password for test"); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("successful login did not reset username failure budget: %v", err)
		}
	})

	t.Run("unknown username", func(t *testing.T) {
		service, _, _ := newAuth(t)
		for i := 1; i <= 20; i++ {
			ctx := auth.WithLoginPeer(context.Background(), fmt.Sprintf("198.18.0.%d:1234", i))
			if _, err := service.Login(ctx, "missing-user", "wrong password for test"); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("unknown-user verification %d error = %v, want ErrUnauthenticated", i, err)
			}
		}
		ctx := auth.WithLoginPeer(context.Background(), "198.18.1.1:1234")
		if _, err := service.Login(ctx, "missing-user", "wrong password for test"); !errors.Is(err, auth.ErrRateLimited) {
			t.Fatalf("21st unknown-user verification error = %v, want ErrRateLimited", err)
		}
	})
}

func TestLoginRehashCASConflictCreatesNoSession(t *testing.T) {
	service, db, now := newAuth(t)
	ctx := auth.WithLoginPeer(context.Background(), "192.0.2.12:1234")
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	legacy := encodedPassword(initialPassword, 19456, 1, 2)
	if _, err := db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", legacy, user.ID); err != nil {
		t.Fatal(err)
	}
	concurrentHash, err := auth.HashPassword("concurrent replacement password", rand.NewChaCha8([32]byte{61}))
	if err != nil {
		t.Fatal(err)
	}
	random := newBlockingReader(62)
	service = auth.NewService(db, auth.Options{Now: func() time.Time { return *now }, Random: random})

	result := make(chan error, 1)
	go func() {
		_, err := service.Login(ctx, "admin", initialPassword)
		result <- err
	}()
	<-random.entered
	if _, err := db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", concurrentHash, user.ID); err != nil {
		close(random.release)
		t.Fatal(err)
	}
	close(random.release)
	if err := <-result; !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("rehash CAS conflict error = %v, want ErrUnauthenticated", err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", user.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != concurrentHash {
		t.Fatal("failed login rehash CAS overwrote the concurrent password")
	}
	var sessions int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE user_id = ?", user.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("failed login rehash CAS created %d sessions", sessions)
	}
}

func TestVerifyPasswordRejectsOutOfBoundsAndMalformedParameters(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
	}{
		{"memory below minimum", encodedPassword(initialPassword, 19455, 2, 1)},
		{"memory above maximum", encodedPassword(initialPassword, 262145, 1, 1)},
		{"iterations below minimum", encodedPassword(initialPassword, 32768, 0, 1)},
		{"iterations above maximum", encodedPassword(initialPassword, 19456, 11, 1)},
		{"parallelism below minimum", encodedPassword(initialPassword, 32768, 2, 0)},
		{"parallelism above maximum", encodedPassword(initialPassword, 19456, 1, 5)},
		{"numeric trailing data", strings.Replace(encodedPassword(initialPassword, 32768, 2, 1), "m=32768,t=2,p=1", "m=32768,t=2,p=1x", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ok, err := auth.VerifyPassword(initialPassword, tt.encoded); err == nil || ok {
				t.Fatalf("VerifyPassword accepted unsafe encoding: ok=%t err=%v", ok, err)
			}
		})
	}
}

func encodedPassword(password string, memory, iterations uint32, parallelism uint8) string {
	salt := []byte("0123456789abcdef")
	// Avoid invoking Argon2 with deliberately unsafe test parameters.
	digest := make([]byte, 32)
	if memory >= 19456 && memory <= 262144 && iterations >= 1 && iterations <= 10 && parallelism >= 1 && parallelism <= 4 {
		digest = argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, 32)
	}
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, iterations, parallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest))
}

func TestLegitimateLoginWorksAfterAdmissionBudgetRefills(t *testing.T) {
	service, _, now := newAuth(t)
	ctx := auth.WithLoginPeer(context.Background(), "192.0.2.99:1234")
	if _, err := service.BootstrapAdministrator(ctx, "admin", initialPassword); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := service.Login(ctx, "admin", initialPassword); err != nil {
			t.Fatalf("legitimate login failed: %v", err)
		}
	}
	if _, err := service.Login(ctx, "admin", initialPassword); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("exhausted pair budget admitted login: %v", err)
	}
	*now = now.Add(30 * time.Second)
	session, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatalf("legitimate login failed after refill: %v", err)
	}
	if user, err := service.Authenticate(ctx, session.Token); err != nil || user.Username != "admin" {
		t.Fatalf("refilled login produced invalid session: user=%+v err=%v", user, err)
	}
}
