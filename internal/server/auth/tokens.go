package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

const ErrAPITokenLimit serviceError = "API token limit reached"

// APIToken 仅含可重复读取的元数据。明文只由创建方法单独返回。
type APIToken struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	CreatedBy         string  `json:"created_by"`
	CreatedByUsername string  `json:"created_by_username"`
	CreatedAt         string  `json:"created_at"`
	ExpiresAt         *string `json:"expires_at"`
	LastUsedAt        *string `json:"last_used_at"`
}

func (s *Service) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.name,t.created_by,u.username,t.created_at,t.expires_at,t.last_used_at FROM api_tokens t JOIN users u ON u.id=t.created_by ORDER BY t.created_at DESC,t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := make([]APIToken, 0)
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedBy, &t.CreatedByUsername, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

func (s *Service) CreateAPIToken(ctx context.Context, userID, sessionToken, name string, days *int, password string) (APIToken, string, error) {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || len(name) > 100 || (days != nil && *days != 30 && *days != 90 && *days != 365) {
		return APIToken{}, "", ErrInvalidInput
	}
	if err := s.acquire(ctx); err != nil {
		return APIToken{}, "", err
	}
	defer func() { <-s.gate }()
	hash, err := s.accountPassword(ctx, userID, password)
	if err != nil {
		return APIToken{}, "", err
	}
	var raw [32]byte
	if _, err = io.ReadFull(s.random, raw[:]); err != nil {
		return APIToken{}, "", errors.New("generate API token secret")
	}
	plaintext := "obt_" + base64.RawURLEncoding.EncodeToString(raw[:])
	digest := sha256.Sum256([]byte(plaintext))
	id, err := uuid.UUID(s.random)
	if err != nil {
		return APIToken{}, "", errors.New("generate API token id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return APIToken{}, "", err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	if err = requireTOTPSession(ctx, tx, userID, sessionToken, now); err != nil {
		return APIToken{}, "", err
	}
	var username string
	var restricted bool
	err = tx.QueryRowContext(ctx, `SELECT username,must_change_password FROM users WHERE id=? AND password_hash=?`, userID, hash).Scan(&username, &restricted)
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, "", ErrWrongPassword
	}
	if err != nil {
		return APIToken{}, "", err
	}
	if restricted {
		return APIToken{}, "", ErrPasswordChangeRequired
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens`).Scan(&count); err != nil {
		return APIToken{}, "", err
	}
	if count >= 20 {
		return APIToken{}, "", ErrAPITokenLimit
	}
	token := APIToken{ID: id, Name: name, CreatedBy: userID, CreatedByUsername: username, CreatedAt: timekey.Format(now)}
	if days != nil {
		expiry := timekey.Format(now.Add(time.Duration(*days) * 24 * time.Hour))
		token.ExpiresAt = &expiry
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO api_tokens(id,name,token_hash,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?)`, id, name, digest[:], userID, token.CreatedAt, token.ExpiresAt); err != nil {
		return APIToken{}, "", err
	}
	if err = audit.InsertDetails(audit.WithActor(ctx, userID), tx, "api_token.create", id, now, audit.Details{"id": id, "name": name}); err != nil {
		return APIToken{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return APIToken{}, "", err
	}
	return token, plaintext, nil
}

func (s *Service) RevokeAPIToken(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(ctx, `DELETE FROM api_tokens WHERE id=? RETURNING name`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = audit.InsertDetails(ctx, tx, "api_token.revoke", id, s.now(), audit.Details{"id": id, "name": name}); err != nil {
		return err
	}
	return tx.Commit()
}

// AuthenticateAPIToken 不读取浏览器会话；过期和无效凭据使用同一错误。
func (s *Service) AuthenticateAPIToken(ctx context.Context, token string) error {
	if len(token) != 47 || !strings.HasPrefix(token, "obt_") {
		return ErrUnauthenticated
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token[4:])
	if err != nil || len(raw) != 32 {
		return ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var id string
	var expiry, lastUsed *string
	var restricted bool
	err = s.db.QueryRowContext(ctx, `SELECT t.id,t.expires_at,t.last_used_at,u.must_change_password FROM api_tokens t JOIN users u ON u.id=t.created_by WHERE t.token_hash=?`, digest[:]).Scan(&id, &expiry, &lastUsed, &restricted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	now := s.now().UTC()
	if expiry != nil {
		at, e := timekey.Parse(*expiry)
		if e != nil || !now.Before(at) {
			return ErrUnauthenticated
		}
	}
	if restricted {
		return ErrPasswordChangeRequired
	}
	cutoff := timekey.Format(now.Add(-5 * time.Minute))
	if lastUsed == nil || *lastUsed <= cutoff {
		s.recordAPITokenUse(id, now, cutoff)
	}
	return nil
}

// recordAPITokenUse 在后台写入，读取请求不等待主库写锁。
// 同一时刻只写一次；被跳过的令牌由后续请求补上。
func (s *Service) recordAPITokenUse(id string, now time.Time, cutoff string) {
	if !s.tokenUseBusy.CompareAndSwap(false, true) {
		return
	}
	s.tokenUses.Add(1)
	go func() {
		defer s.tokenUses.Done()
		defer s.tokenUseBusy.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=? AND (last_used_at IS NULL OR last_used_at<=?)`, timekey.Format(now), id, cutoff)
	}()
}
