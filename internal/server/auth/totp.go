package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpCode(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 15
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}

func matchTOTP(secret []byte, code string, now time.Time, last int64) (int64, bool) {
	matched := int64(0)
	for delta := int64(-1); delta <= 1; delta++ {
		step := now.Unix()/30 + delta
		equal := hmac.Equal([]byte(totpCode(secret, step)), []byte(code))
		if equal && step > last && step > matched {
			matched = step
		}
	}
	return matched, matched > 0
}

// 验证器常把验证码显示成 "123 456"。
func normalizeOTP(otp string) string { return strings.Join(strings.Fields(otp), "") }

func recoveryHash(code string) [32]byte {
	return sha256.Sum256([]byte(strings.ToLower(strings.ReplaceAll(normalizeOTP(code), "-", ""))))
}

type totpState struct {
	cipher, pending []byte
	pendingAt       sql.NullString
	last            int64
}

func (s *Service) readTOTP(ctx context.Context, id string) (totpState, error) {
	var state totpState
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return totpState{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	err := lease.QueryRowContext(ctx, `SELECT totp_ciphertext,totp_pending_ciphertext,totp_pending_at,totp_last_step FROM users WHERE id=?`, id).Scan(&state.cipher, &state.pending, &state.pendingAt, &state.last)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return state, err
}

type secondFactor struct {
	kind string
	step int64
	hash [32]byte
}

func (s *Service) totpAllowed(id string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	return loginBucketFor(s.totpFailures, id, s.totpFailureOverflow, s.now(), 10, 1.0/300).tokens >= 1
}
func (s *Service) secondFactorFailure(id, kind string) {
	if kind != "totp" {
		return
	}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	bucket := loginBucketFor(s.totpFailures, id, s.totpFailureOverflow, now, 10, 1.0/300)
	if bucket.tokens >= 1 {
		bucket.tokens--
	}
	storeLoginBucket(s.totpFailures, id, &s.totpFailureOverflow, bucket, now, 10, 1.0/300)
}

// 失败额度保护的是旧密钥。密钥作废后保留额度，只会挡住刚被重置、正在重新启用的用户。
func (s *Service) clearTOTPFailures(id string) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	delete(s.totpFailures, id)
}
func (s *Service) verifySecondFactor(id string, state totpState, otp string) (secondFactor, error) {
	if state.cipher == nil {
		return secondFactor{}, nil
	}
	if s.box == nil {
		return secondFactor{}, ErrTOTPUnavailable
	}
	if otp == "" {
		return secondFactor{}, ErrTOTPRequired
	}
	if len(otp) > 64 {
		return secondFactor{}, ErrTOTPInvalid
	}
	code := normalizeOTP(otp)
	numeric := len(code) == 6
	for _, c := range code {
		if c < '0' || c > '9' {
			numeric = false
		}
	}
	if !numeric {
		return secondFactor{kind: "recovery", hash: recoveryHash(code)}, nil
	}
	if !s.totpAllowed(id) {
		return secondFactor{}, ErrRateLimited
	}
	secret, err := s.box.Open("totp:"+id, state.cipher)
	if err != nil || len(secret) != 20 {
		clear(secret)
		return secondFactor{}, ErrTOTPUnavailable
	}
	defer clear(secret)
	step, ok := matchTOTP(secret, code, s.now(), state.last)
	if !ok {
		s.secondFactorFailure(id, "totp")
		return secondFactor{}, ErrTOTPInvalid
	}
	return secondFactor{kind: "totp", step: step}, nil
}
func (s *Service) consumeSecondFactor(ctx context.Context, tx *storage.Tx, id string, state totpState, factor secondFactor) error {
	var result sql.Result
	var err error
	equal := " IS "
	if tx.Dialect() == storage.MySQL {
		equal = " <=> "
	}
	switch factor.kind {
	case "totp":
		result, err = tx.ExecContext(ctx, `UPDATE users SET totp_last_step=? WHERE id=? AND totp_last_step<? AND totp_ciphertext`+equal+`?`, factor.step, id, factor.step, state.cipher)
	case "recovery":
		result, err = tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id=? AND code_hash=? AND EXISTS(SELECT 1 FROM users WHERE id=? AND totp_ciphertext`+equal+`?)`, id, factor.hash[:], id, state.cipher)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTOTPInvalid
	}
	return nil
}

type TOTPStatus struct {
	Enabled                bool `json:"enabled"`
	RecoveryCodesRemaining int  `json:"recovery_codes_remaining"`
}

func (s *Service) TOTPStatus(ctx context.Context, id string) (TOTPStatus, error) {
	var status TOTPStatus
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return TOTPStatus{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	err := lease.QueryRowContext(ctx, `SELECT totp_ciphertext IS NOT NULL,(SELECT count(*) FROM recovery_codes WHERE user_id=users.id) FROM users WHERE id=?`, id).Scan(&status.Enabled, &status.RecoveryCodesRemaining)
	return status, err
}

type TOTPSetup struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
	QR     struct {
		Size    int    `json:"size"`
		Modules string `json:"modules"`
	} `json:"qr"`
}

func (s *Service) accountPassword(ctx context.Context, id, password string) (string, error) {
	if len(password) > 1024 {
		return "", ErrInvalidInput
	}
	if err := s.admitPasswordChange(id); err != nil {
		return "", err
	}
	var hash string
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return "", leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if err := lease.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, id).Scan(&hash); err != nil {
		return "", err
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		return "", ErrWrongPassword
	}
	return hash, nil
}
func requireChanged(result sql.Result, err error, conflict error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return conflict
	}
	return nil
}
func (s *Service) SetupTOTP(ctx context.Context, id, password string) (TOTPSetup, error) {
	var setup TOTPSetup
	if s.box == nil {
		return setup, ErrTOTPUnavailable
	}
	if err := s.acquire(ctx); err != nil {
		return setup, err
	}
	defer func() { <-s.gate }()
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return TOTPSetup{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	hash, err := s.accountPassword(ctx, id, password)
	if err != nil {
		return setup, err
	}
	state, err := s.readTOTP(ctx, id)
	if err != nil {
		return setup, err
	}
	if state.cipher != nil {
		return setup, ErrTOTPAlreadyEnabled
	}
	secret := make([]byte, 20)
	defer clear(secret)
	if _, err = io.ReadFull(s.random, secret); err != nil {
		return setup, ErrTOTPUnavailable
	}
	cipher, err := s.box.Seal("totp:"+id, secret)
	if err != nil {
		return setup, ErrTOTPUnavailable
	}
	var username string
	if err = lease.QueryRowContext(ctx, `SELECT username FROM users WHERE id=?`, id).Scan(&username); err != nil {
		return setup, err
	}
	setup.Secret = totpEncoding.EncodeToString(secret)
	setup.URI = totpURI(username, setup.Secret)
	code, err := qr.Encode(setup.URI, qr.M)
	if err != nil {
		return TOTPSetup{}, ErrInvalidInput
	}
	setup.QR.Size = code.Size
	modules := make([]byte, code.Size*code.Size)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			modules[y*code.Size+x] = '0'
			if code.Black(x, y) {
				modules[y*code.Size+x] = '1'
			}
		}
	}
	setup.QR.Modules = string(modules)
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return TOTPSetup{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET totp_pending_ciphertext=?,totp_pending_at=? WHERE id=? AND password_hash=? AND totp_ciphertext IS NULL`, cipher, timekey.Format(s.now()), id, hash)
	if err = requireChanged(result, err, ErrTOTPAlreadyEnabled); err != nil {
		return TOTPSetup{}, err
	}
	if err := tx.Commit(); err != nil {
		return TOTPSetup{}, err
	}
	return setup, nil
}
func totpURI(username, secret string) string {
	uri := url.URL{Scheme: "otpauth", Host: "totp", Path: "/OrbitBeacon:" + username, RawPath: "/OrbitBeacon:" + url.PathEscape(username)}
	uri.RawQuery = url.Values{"secret": {secret}, "issuer": {"OrbitBeacon"}}.Encode()
	return uri.String()
}

func (s *Service) newRecoveryCodes() ([]string, error) {
	codes := make([]string, 10)
	for i := range codes {
		var raw [13]byte
		if _, err := io.ReadFull(s.random, raw[:]); err != nil {
			return nil, ErrTOTPUnavailable
		}
		text := strings.ToLower(totpEncoding.EncodeToString(raw[:]))[:20]
		codes[i] = text[:5] + "-" + text[5:10] + "-" + text[10:15] + "-" + text[15:]
	}
	return codes, nil
}
func replaceRecoveryCodes(ctx context.Context, tx *storage.Tx, id string, codes []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id=?`, id); err != nil {
		return err
	}
	for _, code := range codes {
		hash := recoveryHash(code)
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes(user_id,code_hash) VALUES(?,?)`, id, hash[:]); err != nil {
			return err
		}
	}
	return nil
}
func requireTOTPSession(ctx context.Context, tx *storage.Tx, id, token string, now time.Time) error {
	digest := sha256.Sum256([]byte(token))
	var present int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE user_id=? AND token_hash=? AND expires_at>?`, id, digest[:], now.UnixNano()).Scan(&present)
	if err == sql.ErrNoRows {
		return ErrUnauthenticated
	}
	return err
}

func keepSession(ctx context.Context, tx *storage.Tx, id, token string) error {
	digest := sha256.Sum256([]byte(token))
	_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, id, digest[:])
	return err
}
func (s *Service) ConfirmTOTP(ctx context.Context, id, token, code string) ([]string, error) {
	if s.box == nil {
		return nil, ErrTOTPUnavailable
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	state, err := s.readTOTP(ctx, id)
	if err != nil {
		return nil, err
	}
	at, err := timekey.Parse(state.pendingAt.String)
	if err != nil || state.pending == nil || state.cipher != nil || s.now().Sub(at) > 10*time.Minute || s.now().Before(at) {
		return nil, ErrTOTPSetupExpired
	}
	if !s.totpAllowed(id) {
		return nil, ErrRateLimited
	}
	secret, err := s.box.Open("totp:"+id, state.pending)
	if err != nil || len(secret) != 20 {
		clear(secret)
		return nil, ErrTOTPUnavailable
	}
	defer clear(secret)
	step, ok := matchTOTP(secret, normalizeOTP(code), s.now(), state.last)
	if len(code) > 64 || !ok {
		s.secondFactorFailure(id, "totp")
		return nil, ErrTOTPInvalid
	}
	codes, err := s.newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = requireTOTPSession(ctx, tx, id, token, s.now()); err != nil {
		return nil, err
	}
	equal := " IS "
	if tx.Dialect() == storage.MySQL {
		equal = " <=> "
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET totp_ciphertext=totp_pending_ciphertext,totp_pending_ciphertext=NULL,totp_pending_at=NULL,totp_last_step=? WHERE id=? AND totp_ciphertext IS NULL AND totp_pending_ciphertext`+equal+`? AND totp_pending_at=? AND totp_last_step<?`, step, id, state.pending, state.pendingAt.String, step)
	if err = requireChanged(result, err, ErrTOTPSetupExpired); err != nil {
		return nil, err
	}
	if err = replaceRecoveryCodes(ctx, tx, id, codes); err != nil {
		return nil, err
	}
	if err = keepSession(ctx, tx, id, token); err != nil {
		return nil, err
	}
	if err = audit.Insert(ctx, tx, "user.totp.enable", id, s.now()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return codes, nil
}
func (s *Service) changeTOTP(ctx context.Context, id, token, password, otp string, disable bool) ([]string, error) {
	if s.box == nil {
		return nil, ErrTOTPUnavailable
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	hash, err := s.accountPassword(ctx, id, password)
	if err != nil {
		return nil, err
	}
	state, err := s.readTOTP(ctx, id)
	if err != nil {
		return nil, err
	}
	if state.cipher == nil {
		return nil, ErrTOTPInvalid
	}
	var username string
	if err = lease.QueryRowContext(ctx, `SELECT username FROM users WHERE id=?`, id).Scan(&username); err != nil {
		return nil, err
	}
	factor, err := s.verifySecondFactor(id, state, otp)
	if err != nil {
		if err == ErrTOTPInvalid {
			s.recordLoginFailure(username)
		}
		return nil, err
	}
	var codes []string
	if !disable {
		codes, err = s.newRecoveryCodes()
		if err != nil {
			return nil, err
		}
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = requireTOTPSession(ctx, tx, id, token, s.now()); err != nil {
		return nil, err
	}
	if err = s.consumeSecondFactor(ctx, tx, id, state, factor); err != nil {
		if err == ErrTOTPInvalid {
			s.secondFactorFailure(id, factor.kind)
			s.recordLoginFailure(username)
		}
		return nil, err
	}
	equal := " IS "
	if tx.Dialect() == storage.MySQL {
		equal = " <=> "
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET totp_last_step=totp_last_step WHERE id=? AND password_hash=? AND totp_ciphertext`+equal+`?`, id, hash, state.cipher)
	if err = requireChanged(result, err, ErrWrongPassword); err != nil {
		return nil, err
	}
	action := "user.totp.recovery_codes"
	if disable {
		action = "user.totp.disable"
		if _, err = tx.ExecContext(ctx, `UPDATE users SET totp_ciphertext=NULL,totp_pending_ciphertext=NULL,totp_pending_at=NULL,totp_last_step=0 WHERE id=?`, id); err != nil {
			return nil, err
		}
		if err = keepSession(ctx, tx, id, token); err != nil {
			return nil, err
		}
	}
	if err = replaceRecoveryCodes(ctx, tx, id, codes); err != nil {
		return nil, err
	}
	if err = audit.Insert(ctx, tx, action, id, s.now()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if disable {
		s.clearTOTPFailures(id)
	}
	return codes, nil
}
func (s *Service) DisableTOTP(ctx context.Context, id, token, password, otp string) error {
	_, err := s.changeTOTP(ctx, id, token, password, otp, true)
	return err
}
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, id, token, password, otp string) ([]string, error) {
	return s.changeTOTP(ctx, id, token, password, otp, false)
}

func resetTOTP(ctx context.Context, tx *storage.Tx, id string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE users SET totp_ciphertext=NULL,totp_pending_ciphertext=NULL,totp_pending_at=NULL,totp_last_step=0 WHERE id=?`, id)
	if err = requireChanged(result, err, ErrNotFound); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE created_by=?`, id); err != nil {
		return err
	}
	return audit.Insert(ctx, tx, "user.totp.reset", id, now)
}
func ResetTOTP(ctx context.Context, db *storage.Database, username string, now time.Time) error {
	lease, leaseErr := db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username=?`, username).Scan(&id); err == sql.ErrNoRows {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err = resetTOTP(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) ResetUserTOTP(ctx context.Context, actorID, token, targetID, password string) error {
	if s.box == nil {
		return ErrTOTPUnavailable
	}
	if actorID == targetID {
		return ErrInvalidInput
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	hash, err := s.accountPassword(ctx, actorID, password)
	if err != nil {
		return err
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var present int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id=? AND password_hash=?`, actorID, hash).Scan(&present); err == sql.ErrNoRows {
		return ErrWrongPassword
	} else if err != nil {
		return err
	}
	now := s.now()
	if err = requireTOTPSession(ctx, tx, actorID, token, now); err != nil {
		return err
	}
	if err = resetTOTP(ctx, tx, targetID, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.clearTOTPFailures(targetID)
	return nil
}
