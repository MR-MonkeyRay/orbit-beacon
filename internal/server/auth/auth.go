package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/management"
)

type serviceError string

func (err serviceError) Error() string { return string(err) }

const (
	RoleAdmin                        = "admin"
	RoleViewer                       = "viewer"
	ErrAdminExists      serviceError = "administrator already exists"
	ErrUnauthenticated  serviceError = "authentication required"
	ErrRateLimited      serviceError = "login rate limit reached"
	ErrInvalidInput     serviceError = "invalid user input"
	ErrNotFound         serviceError = "user not found"
	ErrLastAdmin        serviceError = "at least one administrator is required"
	ErrUsernameConflict serviceError = "username already exists"
	SessionLifetime                  = 12 * time.Hour
)

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

type Session struct {
	Token     string    `json:"-"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      User      `json:"user"`
}

type Options struct {
	Now    func() time.Time
	Random io.Reader
}

type Service struct {
	db     *sql.DB
	now    func() time.Time
	random io.Reader
	// The gate bounds expensive password calculations to one per service and
	// protects injectable entropy readers, which need not be concurrency-safe.
	gate       chan struct{}
	waiters    chan struct{}
	rateMu     sync.Mutex
	loginPeers map[netip.Addr]loginBucket
	loginNames map[loginIdentityKey]loginBucket
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	return &Service{db: db, now: opts.Now, random: opts.Random, gate: make(chan struct{}, 1), waiters: make(chan struct{}, 8), loginPeers: make(map[netip.Addr]loginBucket), loginNames: make(map[loginIdentityKey]loginBucket)}
}

func (s *Service) Management() *management.Service {
	return management.NewService(s.db)
}

func HashPassword(password string, random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	var salt [16]byte
	if _, err := io.ReadFull(random, salt[:]); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	digest := argon2.IDKey([]byte(password), salt[:], 2, 32*1024, 1, 32)
	return "$argon2id$v=19$m=32768,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(digest), nil
}

func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// Refuse unbounded work from corrupted or attacker-supplied encodings.
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=32768,t=2,p=1" {
		return false, errors.New("invalid password encoding")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false, errors.New("invalid password salt")
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(digest) != 32 {
		return false, errors.New("invalid password digest")
	}
	actual := argon2.IDKey([]byte(password), salt, 2, 32*1024, 1, 32)
	return subtle.ConstantTimeCompare(actual, digest) == 1, nil
}

func (s *Service) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	default:
	}
	select {
	case s.waiters <- struct{}{}:
		defer func() { <-s.waiters }()
	default:
		return ErrRateLimited
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrRateLimited
	}
}

func validUsername(username string) bool {
	return len(username) > 0 && len(username) <= 128 && strings.TrimSpace(username) != ""
}

func validUser(username, password, role string) bool {
	return validUsername(username) && len(password) >= 8 && len(password) <= 1024 && (role == RoleAdmin || role == RoleViewer)
}

func (s *Service) BootstrapAdministrator(ctx context.Context, username, password string) (User, error) {
	if err := s.acquire(ctx); err != nil {
		return User{}, err
	}
	defer func() { <-s.gate }()
	if !validUser(username, password, RoleAdmin) {
		return User{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role = ?", RoleAdmin).Scan(&count); err != nil {
		return User{}, err
	}
	if count != 0 {
		return User{}, ErrAdminExists
	}
	user, err := s.insertUser(ctx, tx, username, password, RoleAdmin)
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Service) CreateUser(ctx context.Context, username, password, role string) (User, error) {
	if !validUser(username, password, role) {
		return User{}, ErrInvalidInput
	}
	if err := s.acquire(ctx); err != nil {
		return User{}, err
	}
	defer func() { <-s.gate }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	user, err := s.insertUser(ctx, tx, username, password, role)
	if err != nil {
		return User{}, err
	}
	if err := audit.Insert(ctx, tx, "user.create", user.ID, user.CreatedAt); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Service) insertUser(ctx context.Context, tx *sql.Tx, username, password, role string) (User, error) {
	if err := requireAvailableUsername(ctx, tx, username, ""); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password, s.random)
	if err != nil {
		return User{}, err
	}
	id, err := identity.UUID(s.random)
	if err != nil {
		return User{}, err
	}
	user := User{ID: id, Username: username, Role: role, CreatedAt: s.now().UTC()}
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)", id, username, hash, role, user.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	if !validUsername(username) || len(password) < 8 || len(password) > 1024 {
		return Session{}, ErrUnauthenticated
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if err := s.admitLogin(ctx, username); err != nil {
		return Session{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return Session{}, err
	}
	defer func() { <-s.gate }()
	var user User
	var hash, created string
	err := s.db.QueryRowContext(ctx, "SELECT id, username, role, created_at, password_hash FROM users WHERE username = ?", username).Scan(&user.ID, &user.Username, &user.Role, &created, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Unknown usernames consume the same Argon2 work as incorrect passwords.
		var salt [16]byte
		_ = argon2.IDKey([]byte(password), salt[:], 2, 32*1024, 1, 32)
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		return Session{}, ErrUnauthenticated
	}
	user.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Session{}, err
	}
	token, err := s.token()
	if err != nil {
		return Session{}, err
	}
	csrf, err := s.token()
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	session := Session{Token: token, CSRFToken: csrf, ExpiresAt: now.Add(SessionLifetime), User: user}
	digest, csrfDigest := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(csrf))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	// A password or role change from another service must not race stale
	// verified credentials into a new session after revocation has committed.
	result, err := tx.ExecContext(ctx, "INSERT INTO sessions(token_hash, user_id, csrf_hash, expires_at) SELECT ?, id, ?, ? FROM users WHERE id = ? AND password_hash = ? AND role = ?", digest[:], csrfDigest[:], session.ExpiresAt.UnixNano(), user.ID, hash, user.Role)
	if err != nil {
		return Session{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if count != 1 {
		return Session{}, ErrUnauthenticated
	}
	// Reclaim a bounded batch only when a new session can commit successfully.
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at <= ? ORDER BY expires_at LIMIT 128)", now.UnixNano()); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Service) token() (string, error) {
	var bytes [32]byte
	if _, err := io.ReadFull(s.random, bytes[:]); err != nil {
		return "", fmt.Errorf("generate session secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if len(token) != 43 {
		return User{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var user User
	var created string
	var expiry int64
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.username, u.role, u.created_at, s.expires_at
 FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, digest[:]).Scan(&user.ID, &user.Username, &user.Role, &created, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUnauthenticated
	}
	if err != nil {
		return User{}, err
	}
	if s.now().UnixNano() >= expiry {
		return User{}, ErrUnauthenticated
	}
	user.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return user, err
}

// CleanupExpiredSessions removes at most one bounded batch. Callers should use
// it only after successful authentication so unauthenticated traffic cannot
// create arbitrary write load.
func (s *Service) CleanupExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at <= ? ORDER BY expires_at LIMIT 128)", s.now().UnixNano())
	return err
}

func (s *Service) VerifyCSRF(ctx context.Context, token, csrf string) (bool, error) {
	if len(csrf) != 43 {
		return false, nil
	}
	digest := sha256.Sum256([]byte(token))
	var expected []byte
	err := s.db.QueryRowContext(ctx, "SELECT csrf_hash FROM sessions WHERE token_hash = ? AND expires_at > ?", digest[:], s.now().UnixNano()).Scan(&expected)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	actual := sha256.Sum256([]byte(csrf))
	return subtle.ConstantTimeCompare(actual[:], expected) == 1, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	digest := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", digest[:])
	return err
}

func (s *Service) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	if len(newPassword) < 8 || len(newPassword) > 1024 || len(oldPassword) > 1024 {
		return ErrInvalidInput
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hash string
	if err := tx.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", userID).Scan(&hash); err != nil {
		return err
	}
	ok, err := VerifyPassword(oldPassword, hash)
	if err != nil || !ok {
		return ErrUnauthenticated
	}
	hash, err = HashPassword(newPassword, s.random)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", hash, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "user.password.change", userID, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}
