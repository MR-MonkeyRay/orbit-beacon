package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/timekey"
)

const (
	ErrInvalidToken  serviceError = "invalid API token"
	ErrTokenCapacity serviceError = "maximum of 50 active API tokens reached"
)

type APIToken struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Prefix            string     `json:"prefix"`
	CreatedBy         string     `json:"created_by"`
	CreatedByUsername string     `json:"created_by_username"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         *time.Time `json:"expires_at"`
	LastUsedAt        *time.Time `json:"last_used_at"`
	RevokedAt         *time.Time `json:"revoked_at"`
}

type CreatedAPIToken struct {
	APIToken
	Token string `json:"token"`
}

const apiTokenColumns = `t.id,t.name,t.prefix,t.created_by,u.username,t.created_at,t.expires_at,t.last_used_at,t.revoked_at`

type tokenScanner interface{ Scan(...any) error }

func scanAPIToken(row tokenScanner) (APIToken, error) {
	var token APIToken
	var created string
	var expires, used, revoked sql.NullString
	err := row.Scan(&token.ID, &token.Name, &token.Prefix, &token.CreatedBy, &token.CreatedByUsername, &created, &expires, &used, &revoked)
	if err != nil {
		return token, err
	}
	token.CreatedAt, err = timekey.Parse(created)
	if err != nil {
		return token, err
	}
	for _, field := range []struct {
		raw sql.NullString
		dst **time.Time
	}{{expires, &token.ExpiresAt}, {used, &token.LastUsedAt}, {revoked, &token.RevokedAt}} {
		if field.raw.Valid {
			parsed, e := timekey.Parse(field.raw.String)
			if e != nil {
				return token, e
			}
			*field.dst = &parsed
		}
	}
	return token, nil
}

func (s *Service) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens t JOIN users u ON u.id=t.created_by ORDER BY t.created_at DESC,t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := make([]APIToken, 0)
	for rows.Next() {
		token, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Service) CreateAPIToken(ctx context.Context, creator, name string, days *int) (CreatedAPIToken, error) {
	var result CreatedAPIToken
	name = strings.TrimSpace(name)
	if !identity.ValidUUID(creator) || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || len(name) < 1 || len(name) > 100 || (days != nil && (*days < 1 || *days > 365)) {
		return result, ErrInvalidInput
	}
	// Use the system CSPRNG independently of password-work admission and injectable readers.
	id, err := identity.UUID(rand.Reader)
	if err != nil {
		return result, err
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return result, err
	}
	plaintext := "obt_" + base64.RawURLEncoding.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(plaintext))
	now := s.now().UTC()
	result.APIToken = APIToken{ID: id, Name: name, Prefix: id[:8], CreatedBy: creator, CreatedAt: now}
	var expiry any
	if days != nil {
		expires := now.Add(time.Duration(*days) * 24 * time.Hour)
		result.ExpiresAt = &expires
		expiry = timekey.Format(expires)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreatedAPIToken{}, err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?)`, timekey.Format(now)).Scan(&count); err != nil {
		return CreatedAPIToken{}, err
	}
	if count >= 50 {
		return CreatedAPIToken{}, ErrTokenCapacity
	}
	if err = tx.QueryRowContext(ctx, `SELECT username FROM users WHERE id=?`, creator).Scan(&result.CreatedByUsername); err != nil {
		return CreatedAPIToken{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO api_tokens(id,name,token_hash,prefix,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`, id, name, digest[:], result.Prefix, creator, timekey.Format(now), expiry); err != nil {
		return CreatedAPIToken{}, err
	}
	if err = audit.Insert(ctx, tx, "api_token.create", id, now); err != nil {
		return CreatedAPIToken{}, err
	}
	if err = tx.Commit(); err != nil {
		return CreatedAPIToken{}, err
	}
	result.Token = plaintext
	return result, nil
}

func (s *Service) RevokeAPIToken(ctx context.Context, id string) error {
	if !identity.ValidUUID(id) {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, timekey.Format(now), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		if err = audit.Insert(ctx, tx, "api_token.revoke", id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AuthenticateAPIToken always consults the database: revocations are never cached.
// Indexed hash lookup avoids comparing plaintext secrets; the returned hash is
// also compared in constant time before accepting the credential. Invalid-peer
// budgets are charged only after rejection, so they cannot lock out valid tokens.
func (s *Service) AuthenticateAPIToken(ctx context.Context, plaintext string) (APIToken, error) {
	invalid := func() (APIToken, error) {
		if err := s.checkInvalidAPIPeer(ctx, true); err != nil {
			return APIToken{}, err
		}
		return APIToken{}, ErrInvalidToken
	}
	if len(plaintext) != 47 || !strings.HasPrefix(plaintext, "obt_") {
		return invalid()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(plaintext[4:])
	if err != nil || len(raw) != 32 {
		return invalid()
	}
	digest := sha256.Sum256([]byte(plaintext))
	var stored []byte
	var id string
	err = s.db.QueryRowContext(ctx, `SELECT id,token_hash FROM api_tokens WHERE token_hash=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?)`, digest[:], timekey.Format(s.now())).Scan(&id, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid()
	}
	if err != nil {
		return APIToken{}, err
	}
	if subtle.ConstantTimeCompare(digest[:], stored) != 1 {
		return invalid()
	}
	if err = s.admitAPIToken(id); err != nil {
		return APIToken{}, err
	}
	// Return only the principal identity. Metadata and timestamp writes are separate.
	return APIToken{ID: id}, nil
}

func (s *Service) checkInvalidAPIPeer(ctx context.Context, charge bool) error {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	peer := loginRatePeer(ctx)
	if s.apiInvalidPeers == nil {
		s.apiInvalidPeers = make(map[netip.Addr]loginBucket)
		s.apiInvalidOverflow = loginBucket{tokens: 30, at: now}
	}
	bucket := loginBucketFor(s.apiInvalidPeers, peer, s.apiInvalidOverflow, now, 30, 1)
	if bucket.tokens < 1 {
		return ErrRateLimited
	}
	if charge {
		bucket.tokens--
		storeLoginBucket(s.apiInvalidPeers, peer, &s.apiInvalidOverflow, bucket, now, 30, 1)
	}
	return nil
}

func (s *Service) admitAPIToken(id string) error {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	if s.apiTokenBuckets == nil {
		s.apiTokenBuckets = make(map[string]loginBucket)
		s.apiTokenOverflow = loginBucket{tokens: 30, at: now}
	}
	bucket := loginBucketFor(s.apiTokenBuckets, id, s.apiTokenOverflow, now, 30, 10)
	if bucket.tokens < 1 {
		return ErrRateLimited
	}
	bucket.tokens--
	storeLoginBucket(s.apiTokenBuckets, id, &s.apiTokenOverflow, bucket, now, 30, 10)
	return nil
}

// TouchAPIToken is best effort. A dedicated connection disables SQLite busy
// waiting, restoring its setting before returning it to the shared pool. The
// conditional update enforces the minute bound across concurrent requests.
func (s *Service) TouchAPIToken(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	defer func() {
		if _, err := conn.ExecContext(context.Background(), `PRAGMA busy_timeout=5000`); err != nil {
			// A connection with uncertain settings must never return to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return
	}
	now := s.now().UTC()
	var used sql.NullString
	if err = conn.QueryRowContext(ctx, `SELECT last_used_at FROM api_tokens WHERE id=?`, id).Scan(&used); err != nil {
		return
	}
	cutoff := timekey.Format(now.Add(-time.Minute))
	if used.Valid && used.String > cutoff {
		return
	}
	_, _ = conn.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?) AND (last_used_at IS NULL OR last_used_at<=?)`, timekey.Format(now), id, timekey.Format(now), cutoff)
}

func (s *Service) CleanupAPITokens(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 500 {
		return 0, ErrInvalidInput
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id IN (SELECT id FROM api_tokens WHERE revoked_at<? OR expires_at<? LIMIT ?)`, timekey.Format(cutoff), timekey.Format(cutoff), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
