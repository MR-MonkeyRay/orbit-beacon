package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
)

const initialPassword = "initial password for regression"

func newAuth(t *testing.T) (*auth.Service, *sql.DB, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service := auth.NewService(db, auth.Options{
		Now: func() time.Time { return now }, Random: rand.NewChaCha8([32]byte{17}),
	})
	return service, db, &now
}

func TestPasswordHashIsSaltedArgon2idWithRequiredCost(t *testing.T) {
	random := rand.NewChaCha8([32]byte{9})
	first, err := auth.HashPassword(initialPassword, random)
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.HashPassword(initialPassword, random)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("equal passwords reused a salt")
	}
	parts := strings.Split(first, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		t.Fatalf("password encoding is not versioned Argon2id: %q", first)
	}
	var memory, iterations, parallelism uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		t.Fatal(err)
	}
	if memory != 32*1024 || iterations != 2 || parallelism != 1 {
		t.Fatalf("Argon2id cost = m=%d,t=%d,p=%d", memory, iterations, parallelism)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 16 {
		t.Fatalf("invalid or too-short salt: length=%d err=%v", len(salt), err)
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(digest) < 32 {
		t.Fatalf("invalid or too-short digest: length=%d err=%v", len(digest), err)
	}
	want := argon2.IDKey([]byte(initialPassword), salt, iterations, memory, uint8(parallelism), uint32(len(digest)))
	if !bytes.Equal(digest, want) {
		t.Fatal("stored digest does not match independent Argon2id calculation")
	}
	for _, tt := range []struct {
		password string
		want     bool
	}{{initialPassword, true}, {"wrong password", false}, {"", false}} {
		got, err := auth.VerifyPassword(tt.password, first)
		if err != nil || got != tt.want {
			t.Errorf("verification result = %t, %v; want %t", got, err, tt.want)
		}
	}
	for _, malformed := range []string{"", "plaintext", "$argon2id$v=19$m=32768,t=2,p=1$invalid!$invalid!"} {
		if ok, _ := auth.VerifyPassword(initialPassword, malformed); ok {
			t.Errorf("accepted malformed password encoding %q", malformed)
		}
	}
}

func TestPasswordHashRefusesFailedEntropy(t *testing.T) {
	if _, err := auth.HashPassword(initialPassword, bytes.NewReader(nil)); err == nil {
		t.Fatal("password hashing succeeded without random salt bytes")
	}
}

func TestBootstrapRefusesToReplaceExistingAdministrator(t *testing.T) {
	service, db, _ := newAuth(t)
	ctx := context.Background()
	original, err := service.BootstrapAdministrator(ctx, "first-admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if original.Role != auth.RoleAdmin || original.ID == "" {
		t.Fatalf("bootstrap user = %+v", original)
	}
	// Reconstructing the service must not reopen the bootstrap window.
	service = auth.NewService(db, auth.Options{
		Now:    func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		Random: rand.NewChaCha8([32]byte{18}),
	})
	for _, username := range []string{"first-admin", "different-admin"} {
		if _, err := service.BootstrapAdministrator(ctx, username, "replacement password"); !errors.Is(err, auth.ErrAdminExists) {
			t.Errorf("second bootstrap error = %v, want ErrAdminExists", err)
		}
	}
	var admins int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role = 'admin'").Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if admins != 1 {
		t.Errorf("administrator count after refused bootstrap = %d", admins)
	}
	var storedHash string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", original.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == initialPassword {
		t.Fatal("bootstrap persisted a plaintext password")
	}
	if ok, err := auth.VerifyPassword(initialPassword, storedHash); err != nil || !ok {
		t.Fatalf("persisted bootstrap password is not verifiable: ok=%t err=%v", ok, err)
	}
	if _, err := service.Login(ctx, "first-admin", initialPassword); err != nil {
		t.Fatalf("refused bootstrap changed original credentials: %v", err)
	}
	if _, err := service.Login(ctx, "first-admin", "replacement password"); err == nil {
		t.Fatal("refused bootstrap installed replacement password")
	}
}

func TestSessionsPersistOnlySHA256AndExpireAfterTwelveHours(t *testing.T) {
	service, db, now := newAuth(t)
	ctx := context.Background()
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.Token == "" || first.CSRFToken == "" || first.CSRFToken == first.Token {
		t.Fatal("sessions need independent nonempty session and CSRF tokens")
	}
	if !first.ExpiresAt.Equal(now.Add(12 * time.Hour)) {
		t.Errorf("expiry = %s, want %s", first.ExpiresAt, now.Add(12*time.Hour))
	}
	rows, err := db.QueryContext(ctx, "SELECT token_hash FROM sessions WHERE user_id = ?", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored [][]byte
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		stored = append(stored, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored session count = %d", len(stored))
	}
	for _, token := range []string{first.Token, second.Token} {
		digest := sha256.Sum256([]byte(token))
		found := false
		for _, value := range stored {
			if bytes.Equal(value, []byte(token)) {
				t.Fatal("stored raw session token")
			}
			found = found || bytes.Equal(value, digest[:]) || string(value) == hex.EncodeToString(digest[:])
		}
		if !found {
			t.Fatal("stored session digest is not SHA-256 of the bearer token")
		}
	}
	*now = first.ExpiresAt.Add(-time.Nanosecond)
	if got, err := service.Authenticate(ctx, first.Token); err != nil || got.ID != user.ID {
		t.Fatalf("valid session rejected before expiry: user=%+v err=%v", got, err)
	}
	*now = first.ExpiresAt
	if _, err := service.Authenticate(ctx, first.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("session accepted at its expiry: %v", err)
	}
}

func TestPasswordChangeRevokesAllSessionsButFailedChangeDoesNot(t *testing.T) {
	service, _, _ := newAuth(t)
	ctx := context.Background()
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Login(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, user.ID, "wrong old password", "new regression password"); err == nil {
		t.Fatal("changed password without proving the old password")
	}
	if _, err := service.Authenticate(ctx, first.Token); err != nil {
		t.Fatalf("failed password change revoked a session: %v", err)
	}
	if err := service.ChangePassword(ctx, user.ID, initialPassword, "new regression password"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first.Token, second.Token} {
		if _, err := service.Authenticate(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("old session survived password change: %v", err)
		}
	}
	if _, err := service.Login(ctx, "admin", initialPassword); err == nil {
		t.Fatal("old password still works")
	}
	if _, err := service.Login(ctx, "admin", "new regression password"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestLoginRefusesFailedSessionEntropy(t *testing.T) {
	service, db, now := newAuth(t)
	ctx := context.Background()
	if _, err := service.BootstrapAdministrator(ctx, "admin", initialPassword); err != nil {
		t.Fatal(err)
	}
	service = auth.NewService(db, auth.Options{
		Now: func() time.Time { return *now }, Random: bytes.NewReader(nil),
	})
	if _, err := service.Login(ctx, "admin", initialPassword); err == nil {
		t.Fatal("login issued a session without random bytes")
	}
	var sessions int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("failed entropy left %d persisted sessions", sessions)
	}
}

func TestValidCredentialsAreNotLockedOutByInvalidAttempts(t *testing.T) {
	service, _, _ := newAuth(t)
	ctx := context.Background()
	if _, err := service.BootstrapAdministrator(ctx, "admin", initialPassword); err != nil {
		t.Fatal(err)
	}
	for range 60 {
		if _, err := service.Login(ctx, "unknown", "invalid password"); !errors.Is(err, auth.ErrUnauthenticated) && !errors.Is(err, auth.ErrRateLimited) {
			t.Fatalf("invalid login error = %v", err)
		}
	}
	if _, err := service.Login(ctx, "admin", initialPassword); err != nil {
		t.Fatalf("invalid attempts locked out valid credentials: %v", err)
	}
}

func TestSuccessfulLoginReclaimsExpiredSessionsAndPreservesActiveSessions(t *testing.T) {
	service, db, now := newAuth(t)
	ctx := context.Background()
	admin, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := service.CreateUser(ctx, "viewer", initialPassword, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.Login(ctx, "viewer", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a backlog without repeated password hashing or pinning a cleanup batch size.
	for i := range 256 {
		digest := sha256.Sum256([]byte(fmt.Sprintf("expired-session-%d", i)))
		owner := admin.ID
		if i%2 == 0 {
			owner = viewer.ID
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES (?,?,?,?)", digest[:], owner, digest[:], now.Add(-time.Hour).UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	boundary := sha256.Sum256([]byte("expires-exactly-now"))
	if _, err := db.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES (?,?,?,?)", boundary[:], admin.ID, boundary[:], now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	// Repeated successful logins must drain even a backlog larger than one batch.
	// Moving the clock avoids coupling this assertion to the login rate limit.
	previous := 257
	for range 257 {
		if _, err := service.Login(ctx, "admin", initialPassword); err != nil {
			t.Fatal(err)
		}
		var expired int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE expires_at <= ?", now.UnixNano()).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired >= previous {
			t.Fatalf("successful login made no progress reclaiming expired sessions: before=%d after=%d", previous, expired)
		}
		previous = expired
		if expired == 0 {
			break
		}
		*now = now.Add(time.Minute)
	}
	var expired int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE expires_at <= ?", now.UnixNano()).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 0 {
		t.Errorf("successful logins left %d expired rows", expired)
	}
	if user, err := service.Authenticate(ctx, active.Token); err != nil || user.ID != viewer.ID {
		t.Errorf("cleanup revoked another user's active session: user=%+v err=%v", user, err)
	}
}

func TestSessionExpirationHasLeadingIndex(t *testing.T) {
	_, db, _ := newAuth(t)
	var indexed int
	// Check index semantics, not a migration's chosen index name or SQL text.
	if err := db.QueryRow(`SELECT count(*) FROM pragma_index_list('sessions') AS indexes
		JOIN pragma_index_info(indexes.name) AS columns
		WHERE columns.seqno = 0 AND columns.name = 'expires_at' AND indexes.partial = 0`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed == 0 {
		t.Error("sessions.expires_at has no leading, nonpartial index for bounded expiry cleanup")
	}
}

func TestLoginAdmissionRejectsBeforeDatabaseAndDoesNotLockOtherPeers(t *testing.T) {
	service, db, _ := newAuth(t)
	if _, err := service.BootstrapAdministrator(context.Background(), "admin", initialPassword); err != nil {
		t.Fatal(err)
	}
	attacker := auth.WithLoginPeer(context.Background(), "192.0.2.1:1000")
	limited := false
	for range 100 {
		_, err := service.Login(attacker, "unknown", "invalid password")
		if errors.Is(err, auth.ErrRateLimited) {
			limited = true
			break
		}
		if !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	if !limited {
		t.Fatal("invalid source never reached computation admission limit")
	}
	otherPeer := auth.WithLoginPeer(context.Background(), "192.0.2.2:1000")
	if _, err := service.Login(otherPeer, "admin", initialPassword); err != nil {
		t.Fatalf("attacker exhausted another peer's valid login: %v", err)
	}
	if _, err := service.Login(attacker, "admin", initialPassword); err != nil {
		t.Fatalf("one invalid username locked every account at the peer: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	samePeerNewPort := auth.WithLoginPeer(context.Background(), "192.0.2.1:9999")
	if _, err := service.Login(samePeerNewPort, "unknown", "invalid password"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("limited login reached unavailable database or bypassed via port: %v", err)
	}
}
