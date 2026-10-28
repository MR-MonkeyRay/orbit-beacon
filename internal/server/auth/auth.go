package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/timekey"
)

type serviceError string

func (err serviceError) Error() string { return string(err) }

const (
	RoleAdmin                        = "admin"
	RoleViewer                       = "viewer"
	ErrAdminExists      serviceError = "administrator already exists"
	ErrUnauthenticated  serviceError = "authentication required"
	ErrWrongPassword    serviceError = "current password is incorrect"
	ErrRateLimited      serviceError = "login rate limit reached"
	ErrInvalidInput     serviceError = "invalid user input"
	ErrNotFound         serviceError = "user not found"
	ErrLastAdmin        serviceError = "at least one administrator is required"
	ErrUsernameConflict serviceError = "username already exists"
	SessionLifetime                  = 12 * time.Hour
)

const (
	passwordMemory      = 32 * 1024
	passwordIterations  = 2
	passwordParallelism = 1
)

var csrfEncoding = base64.RawURLEncoding.Strict()

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
	gate                   chan struct{}
	waiters                chan struct{}
	rateMu                 sync.Mutex
	loginPeers             map[netip.Addr]loginBucket
	loginNames             map[loginIdentityKey]loginBucket
	loginFailures          map[[sha256.Size]byte]loginBucket
	loginSuccesses         map[loginIdentityKey]time.Time
	passwordChanges        map[string]loginBucket
	loginPeerOverflow      loginBucket
	loginNameOverflow      loginBucket
	loginFailureOverflow   loginBucket
	passwordChangeOverflow loginBucket
	apiTokenBuckets        map[string]loginBucket
	apiInvalidPeers        map[netip.Addr]loginBucket
	apiTokenOverflow       loginBucket
	apiInvalidOverflow     loginBucket
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	now := opts.Now()
	return &Service{
		db: db, now: opts.Now, random: opts.Random,
		gate: make(chan struct{}, 1), waiters: make(chan struct{}, 8),
		loginPeers:             make(map[netip.Addr]loginBucket),
		loginNames:             make(map[loginIdentityKey]loginBucket),
		loginFailures:          make(map[[sha256.Size]byte]loginBucket),
		loginSuccesses:         make(map[loginIdentityKey]time.Time),
		passwordChanges:        make(map[string]loginBucket),
		loginPeerOverflow:      loginBucket{tokens: 32, at: now},
		loginNameOverflow:      loginBucket{tokens: 10, at: now},
		loginFailureOverflow:   loginBucket{tokens: 20, at: now},
		passwordChangeOverflow: loginBucket{tokens: 5, at: now},
	}
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
	digest := argon2.IDKey([]byte(password), salt[:], passwordIterations, passwordMemory, passwordParallelism, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemory, passwordIterations, passwordParallelism, base64.RawStdEncoding.EncodeToString(salt[:]), base64.RawStdEncoding.EncodeToString(digest)), nil
}

func VerifyPassword(password, encoded string) (bool, error) {
	matches, _, err := verifyPassword(password, encoded)
	return matches, err
}

// current reports whether a verified encoding needs a parameter upgrade.
func verifyPassword(password, encoded string) (matches, current bool, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, false, errors.New("invalid password encoding")
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return false, false, errors.New("invalid password parameters")
	}
	var costs [3]uint64
	for i, prefix := range []string{"m=", "t=", "p="} {
		value, found := strings.CutPrefix(parameters[i], prefix)
		if !found {
			return false, false, errors.New("invalid password parameters")
		}
		costs[i], err = strconv.ParseUint(value, 10, 32)
		if err != nil {
			return false, false, errors.New("invalid password parameters")
		}
	}
	memory, iterations, parallelism := uint32(costs[0]), uint32(costs[1]), uint8(costs[2])
	// Bound work before Argon2 allocates memory or starts worker goroutines.
	if costs[0] < 19456 || costs[0] > 262144 || costs[1] < 1 || costs[1] > 10 || costs[2] < 1 || costs[2] > 4 {
		return false, false, errors.New("unsafe password parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false, false, errors.New("invalid password salt")
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(digest) != 32 {
		return false, false, errors.New("invalid password digest")
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, 32)
	return subtle.ConstantTimeCompare(actual, digest) == 1, memory == passwordMemory && iterations == passwordIterations && parallelism == passwordParallelism, nil
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
	user, hash, err := s.prepareUser(username, password, RoleAdmin)
	if err != nil {
		return User{}, err
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
	if err := s.insertUser(ctx, tx, user, hash); err != nil {
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
	user, hash, err := s.prepareUser(username, password, role)
	if err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if err := s.insertUser(ctx, tx, user, hash); err != nil {
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

func (s *Service) prepareUser(username, password, role string) (User, string, error) {
	hash, err := HashPassword(password, s.random)
	if err != nil {
		return User{}, "", err
	}
	id, err := identity.UUID(s.random)
	if err != nil {
		return User{}, "", err
	}
	return User{ID: id, Username: username, Role: role, CreatedAt: s.now().UTC()}, hash, nil
}

func (s *Service) insertUser(ctx context.Context, tx *sql.Tx, user User, hash string) error {
	if err := requireAvailableUsername(ctx, tx, user.Username, ""); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO users(id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)", user.ID, user.Username, hash, user.Role, timekey.Format(user.CreatedAt))
	return err
}

func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	if !validUsername(username) || len(password) < 8 || len(password) > 1024 {
		return Session{}, ErrUnauthenticated
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if err := s.checkLogin(ctx, username); err != nil {
		return Session{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return Session{}, err
	}
	defer func() { <-s.gate }()
	// Charge only admitted work, rechecking budgets after queued work completes.
	if err := s.admitLogin(ctx, username); err != nil {
		return Session{}, err
	}
	var user User
	var hash, created string
	err := s.db.QueryRowContext(ctx, "SELECT id, username, role, created_at, password_hash FROM users WHERE username = ?", username).Scan(&user.ID, &user.Username, &user.Role, &created, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Unknown usernames consume the same Argon2 work as incorrect passwords.
		var salt [16]byte
		_ = argon2.IDKey([]byte(password), salt[:], passwordIterations, passwordMemory, passwordParallelism, 32)
		s.recordLoginFailure(ctx, username)
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	ok, current, err := verifyPassword(password, hash)
	if err != nil || !ok {
		s.recordLoginFailure(ctx, username)
		return Session{}, ErrUnauthenticated
	}
	user.CreatedAt, err = timekey.Parse(created)
	if err != nil {
		return Session{}, err
	}
	newHash := hash
	if !current {
		newHash, err = HashPassword(password, s.random)
		if err != nil {
			return Session{}, err
		}
	}
	token, err := s.token()
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	session := Session{Token: token, CSRFToken: sessionCSRF(token), ExpiresAt: now.Add(SessionLifetime), User: user}
	digest := sha256.Sum256([]byte(token))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	if newHash != hash {
		result, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ? AND password_hash = ? AND role = ?", newHash, user.ID, hash, user.Role)
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
	}
	// A password or role change from another service must not race stale
	// verified credentials into a new session after revocation has committed.
	result, err := tx.ExecContext(ctx, "INSERT INTO sessions(token_hash, user_id, expires_at) SELECT ?, id, ? FROM users WHERE id = ? AND password_hash = ? AND role = ?", digest[:], session.ExpiresAt.UnixNano(), user.ID, newHash, user.Role)
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
	details := audit.Details{}
	if peer, _ := ctx.Value(loginPeerKey{}).(netip.Addr); peer.IsValid() {
		details["peer"] = peer.String()
	}
	if err := audit.InsertDetails(audit.WithActor(ctx, user.ID), tx, "session.create", user.ID, now, details); err != nil {
		return Session{}, err
	}
	// Reclaim a bounded batch only when a new session can commit successfully.
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at <= ? ORDER BY expires_at LIMIT 128)", now.UnixNano()); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	s.recordLoginSuccess(ctx, username)
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
	user, _, err := s.lookupSession(ctx, token)
	return user, err
}

// CurrentSession recovers a stable session-bound CSRF token without exposing the
// HttpOnly credential or rotating another tab's token.
func (s *Service) CurrentSession(ctx context.Context, token string) (Session, error) {
	user, expiry, err := s.lookupSession(ctx, token)
	if err != nil {
		return Session{}, err
	}
	return Session{User: user, ExpiresAt: time.Unix(0, expiry).UTC(), CSRFToken: sessionCSRF(token)}, nil
}

func (s *Service) lookupSession(ctx context.Context, token string) (User, int64, error) {
	if len(token) != 43 {
		return User{}, 0, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var user User
	var created string
	var expiry int64
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.username, u.role, u.created_at, s.expires_at
 FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, digest[:]).Scan(&user.ID, &user.Username, &user.Role, &created, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, 0, ErrUnauthenticated
	}
	if err != nil {
		return User{}, 0, err
	}
	if s.now().UnixNano() >= expiry {
		return User{}, 0, ErrUnauthenticated
	}
	user.CreatedAt, err = timekey.Parse(created)
	return user, expiry, err
}

// CleanupExpiredSessions removes at most one bounded batch and reports how many
// sessions it removed. The periodic retention task calls it, so request
// handling never takes the SQLite writer just to reclaim sessions.
func (s *Service) CleanupExpiredSessions(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at <= ? ORDER BY expires_at LIMIT 128)", s.now().UnixNano())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Service) VerifyCSRF(ctx context.Context, token, csrf string) (bool, error) {
	if len(token) != 43 || len(csrf) != 43 {
		return false, nil
	}
	var actual [32]byte
	if n, err := csrfEncoding.Decode(actual[:], []byte(csrf)); err != nil || n != len(actual) {
		return false, nil
	}
	digest := sha256.Sum256([]byte(token))
	var present int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE token_hash = ? AND expires_at > ?", digest[:], s.now().UnixNano()).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	expected := csrfDigest(token)
	return subtle.ConstantTimeCompare(actual[:], expected[:]) == 1, nil
}

func csrfDigest(token string) [32]byte {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("orbitbeacon/browser-csrf/v1"))
	var digest [32]byte
	_ = mac.Sum(digest[:0])
	return digest
}

func sessionCSRF(token string) string {
	digest := csrfDigest(token)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (s *Service) Logout(ctx context.Context, token string) error {
	digest := sha256.Sum256([]byte(token))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID string
	err = tx.QueryRowContext(ctx, "DELETE FROM sessions WHERE token_hash = ? RETURNING user_id", digest[:]).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "session.delete", userID, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	if len(newPassword) < 8 || len(newPassword) > 1024 || len(oldPassword) > 1024 {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	// Charge only admitted work: gate rejections and cancellations are free.
	if err := s.admitPasswordChange(userID); err != nil {
		return err
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", userID).Scan(&hash); err != nil {
		return err
	}
	ok, err := VerifyPassword(oldPassword, hash)
	if err != nil || !ok {
		return ErrWrongPassword
	}
	newHash, err := HashPassword(newPassword, s.random)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ? AND password_hash = ?", newHash, userID, hash)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		// A concurrent change invalidated the old credential; roll back and report it.
		return ErrWrongPassword
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "user.password.change", userID, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}
