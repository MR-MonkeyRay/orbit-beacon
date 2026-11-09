package auth_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/secretbox"
)

func testOTP(secret string, now time.Time) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := digest[19] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(digest[offset:offset+4])&0x7fffffff)%1000000)
}
func totpFixture(t *testing.T) (*auth.Service, *sql.DB, *time.Time, *secretbox.Box, auth.User, auth.Session) {
	t.Helper()
	_, db, now := newAuth(t)
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon account secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewService(stableDatabase(t, db), auth.Options{Now: func() time.Time { return *now }, SecretBox: box})
	user, err := s.CreateUser(context.Background(), "totp-admin", initialPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Login(context.Background(), user.Username, initialPassword, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	return s, db, now, box, user, session
}
func enableTOTP(t *testing.T, s *auth.Service, user auth.User, session auth.Session, now time.Time) (string, []string) {
	t.Helper()
	ctx := audit.WithActor(context.Background(), user.ID)
	setup, err := s.SetupTOTP(ctx, user.ID, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(setup.Secret) != 32 || setup.QR.Size*setup.QR.Size != len(setup.QR.Modules) || !strings.HasPrefix(setup.URI, "otpauth://totp/OrbitBeacon:") {
		t.Fatal("invalid setup shape")
	}
	codes, err := s.ConfirmTOTP(ctx, user.ID, session.Token, testOTP(setup.Secret, now))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 {
		t.Fatal("recovery count")
	}
	for _, code := range codes {
		if len(code) != 23 {
			t.Fatal("recovery format")
		}
	}
	return setup.Secret, codes
}
func TestTOTPLoginReplayRecoveryAndFailClosed(t *testing.T) {
	s, db, now, box, user, current := totpFixture(t)
	ctx := context.Background()
	other, err := s.Login(ctx, user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	secret, codes := enableTOTP(t, s, user, current, *now)
	if _, err = s.CurrentSession(ctx, current.Token); err != nil {
		t.Fatal("current session revoked", err)
	}
	if _, err = s.CurrentSession(ctx, other.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("other session retained", err)
	}
	var before, after int
	db.QueryRow("SELECT count(*) FROM sessions").Scan(&before)
	if _, err = s.Login(ctx, user.Username, initialPassword, ""); !errors.Is(err, auth.ErrTOTPRequired) {
		t.Fatal(err)
	}
	db.QueryRow("SELECT count(*) FROM sessions").Scan(&after)
	if after != before {
		t.Fatal("required created session")
	}
	if _, err = s.Login(ctx, user.Username, "incorrect password", testOTP(secret, *now)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, user.Username, initialPassword, testOTP(secret, *now)); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatal("confirmation replay", err)
	}
	*now = now.Add(30 * time.Second)
	login, err := s.Login(ctx, user.Username, initialPassword, testOTP(secret, *now))
	if err != nil || !login.User.TOTPEnabled {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, user.Username, initialPassword, testOTP(secret, *now)); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatal("login replay", err)
	}
	if _, err = s.Login(ctx, user.Username, initialPassword, strings.ToUpper(strings.ReplaceAll(codes[0], "-", ""))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, user.Username, initialPassword, codes[0]); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatal("recovery reused", err)
	}
	status, err := s.TOTPStatus(ctx, user.ID)
	if err != nil || !status.Enabled || status.RecoveryCodesRemaining != 9 {
		t.Fatal(status, err)
	}
	noBox := auth.NewService(stableDatabase(t, db), auth.Options{Now: func() time.Time { return *now }})
	if _, err = noBox.Login(ctx, user.Username, initialPassword, codes[1]); !errors.Is(err, auth.ErrTOTPUnavailable) {
		t.Fatal("nil box did not fail closed", err)
	}
	var cipher []byte
	db.QueryRow("SELECT totp_ciphertext FROM users WHERE id=?", user.ID).Scan(&cipher)
	if _, err = box.Open("totp:someone-else", cipher); err == nil {
		t.Fatal("cross-user decryption")
	}
	var events string
	db.QueryRow("SELECT group_concat(details) FROM audit_events").Scan(&events)
	if strings.Contains(events, secret) {
		t.Fatal("secret audit leak")
	}
	for _, code := range codes {
		if strings.Contains(events, code) {
			t.Fatal("recovery audit leak")
		}
	}
	if !strings.Contains(events, `"second_factor":"totp"`) || !strings.Contains(events, `"second_factor":"recovery"`) {
		t.Fatal("missing factor audit")
	}
}
func TestTOTPPendingExpiryAndAlreadyEnabled(t *testing.T) {
	s, _, now, _, user, current := totpFixture(t)
	ctx := context.Background()
	setup, err := s.SetupTOTP(ctx, user.ID, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, user.Username, initialPassword, "arbitrary"); err != nil {
		t.Fatal("pending affected login", err)
	}
	*now = now.Add(10*time.Minute + time.Second)
	if _, err = s.ConfirmTOTP(ctx, user.ID, current.Token, testOTP(setup.Secret, *now)); !errors.Is(err, auth.ErrTOTPSetupExpired) {
		t.Fatal(err)
	}
	enableTOTP(t, s, user, current, *now)
	if _, err = s.SetupTOTP(ctx, user.ID, initialPassword); !errors.Is(err, auth.ErrTOTPAlreadyEnabled) {
		t.Fatal(err)
	}
}
func TestTOTPConcurrentCodeConsumption(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprint(recovery), func(t *testing.T) {
			s, db, now, box, user, current := totpFixture(t)
			secret, codes := enableTOTP(t, s, user, current, *now)
			*now = now.Add(30 * time.Second)
			otp := testOTP(secret, *now)
			if recovery {
				otp = codes[0]
			}
			second := auth.NewService(stableDatabase(t, db), auth.Options{Now: func() time.Time { return *now }, SecretBox: box})
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for _, service := range []*auth.Service{s, second} {
				wg.Add(1)
				go func(service *auth.Service) {
					defer wg.Done()
					_, err := service.Login(context.Background(), user.Username, initialPassword, otp)
					errs <- err
				}(service)
			}
			wg.Wait()
			close(errs)
			success := 0
			for err := range errs {
				if err == nil {
					success++
				} else if !errors.Is(err, auth.ErrTOTPInvalid) {
					t.Fatal(err)
				}
			}
			if success != 1 {
				t.Fatalf("success=%d", success)
			}
		})
	}
}
func TestTOTPDisableRegenerateAndReset(t *testing.T) {
	s, db, now, _, user, current := totpFixture(t)
	ctx := audit.WithActor(context.Background(), user.ID)
	_, codes := enableTOTP(t, s, user, current, *now)
	for _, args := range [][2]string{{"bad password", codes[0]}, {initialPassword, ""}} {
		if err := s.DisableTOTP(ctx, user.ID, current.Token, args[0], args[1]); err == nil {
			t.Fatal("missing credential accepted")
		}
		if _, err := s.RegenerateRecoveryCodes(ctx, user.ID, current.Token, args[0], args[1]); err == nil {
			t.Fatal("regenerate missing credential")
		}
	}
	*now = now.Add(time.Minute)
	fresh, err := s.RegenerateRecoveryCodes(ctx, user.ID, current.Token, initialPassword, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, user.Username, initialPassword, codes[1]); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatal("old recovery retained", err)
	}
	other, err := s.Login(ctx, user.Username, initialPassword, fresh[0])
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err = s.DisableTOTP(ctx, user.ID, current.Token, initialPassword, fresh[1]); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CurrentSession(ctx, current.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CurrentSession(ctx, other.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("disable retained other", err)
	}
	status, _ := s.TOTPStatus(ctx, user.ID)
	if status.Enabled || status.RecoveryCodesRemaining != 0 {
		t.Fatal(status)
	}
	*now = now.Add(time.Minute)
	enableTOTP(t, s, user, current, *now)
	actor, err := s.CreateUser(ctx, "reset-operator", initialPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := s.Login(ctx, actor.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResetUserTOTP(ctx, user.ID, current.Token, user.ID, initialPassword); !errors.Is(err, auth.ErrInvalidInput) {
		t.Fatal(err)
	}
	if err = s.ResetUserTOTP(ctx, actor.ID, operator.Token, user.ID, "bad password"); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatal(err)
	}
	stale, err := s.Login(ctx, actor.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Logout(ctx, stale.Token); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetUserTOTP(ctx, actor.ID, stale.Token, user.ID, initialPassword); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked session reset", err)
	}
	if status, err = s.TOTPStatus(ctx, user.ID); err != nil || !status.Enabled {
		t.Fatal("revoked session changed target", status, err)
	}
	if err = s.ResetUserTOTP(audit.WithActor(context.Background(), actor.ID), actor.ID, operator.Token, user.ID, initialPassword); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CurrentSession(ctx, current.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("reset retained session", err)
	}
	current, err = s.Login(context.Background(), user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	enableTOTP(t, s, user, current, *now)
	system := audit.WithSystemActor(context.Background(), "reset-totp")
	if err = auth.ResetTOTP(system, stableDatabase(t, db), user.Username, *now); err != nil {
		t.Fatal(err)
	}
	if err = auth.ResetTOTP(system, stableDatabase(t, db), "missing", *now); !errors.Is(err, auth.ErrNotFound) {
		t.Fatal(err)
	}
	var count int
	db.QueryRow("SELECT count(*) FROM sessions WHERE user_id=?", user.ID).Scan(&count)
	if count != 0 {
		t.Fatal("offline reset sessions")
	}
	var details string
	db.QueryRow("SELECT details FROM audit_events WHERE action='user.totp.reset' AND actor_id IS NULL").Scan(&details)
	if !strings.Contains(details, "reset-totp") {
		t.Fatal("system audit")
	}
}
func TestTOTPFailureBudgetRejectsValidCodeAndAllowsRecovery(t *testing.T) {
	s, db, now, box, user, current := totpFixture(t)
	secret, codes := enableTOTP(t, s, user, current, *now)
	*now = now.Add(30 * time.Second)
	// 每次用不同来源，隔离现有登录准入预算与第二因素失败预算。
	for i := range 10 {
		peer := auth.WithLoginPeer(context.Background(), fmt.Sprintf("192.0.2.%d:1234", i+1))
		if _, err := s.Login(peer, user.Username, initialPassword, "not-a-recovery-code"); !errors.Is(err, auth.ErrTOTPInvalid) {
			t.Fatal(err)
		}
	}
	// 以一个新服务隔离恢复码无效造成的用户名预算消耗。
	s = auth.NewService(stableDatabase(t, db), auth.Options{Now: func() time.Time { return *now }, SecretBox: box})
	invalidCode := ""
	for candidate := 0; candidate < 10; candidate++ {
		value := fmt.Sprintf("%06d", candidate)
		if value != testOTP(secret, now.Add(-30*time.Second)) && value != testOTP(secret, *now) && value != testOTP(secret, now.Add(30*time.Second)) {
			invalidCode = value
			break
		}
	}
	for i := range 10 {
		peer := auth.WithLoginPeer(context.Background(), fmt.Sprintf("192.0.2.%d:1234", i+1))
		if _, err := s.Login(peer, user.Username, initialPassword, invalidCode); !errors.Is(err, auth.ErrTOTPInvalid) {
			t.Fatal(err)
		}
	}
	peer := auth.WithLoginPeer(context.Background(), "192.0.2.100:1234")
	if _, err := s.Login(peer, user.Username, initialPassword, testOTP(secret, *now)); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatal(err)
	}
	if _, err := s.Login(peer, user.Username, initialPassword, codes[0]); err != nil {
		t.Fatal("recovery locked", err)
	}
	if _, err := s.Login(peer, user.Username, initialPassword, testOTP(secret, *now)); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatal("login success bypass", err)
	}
	*now = now.Add(300 * time.Second)
	if _, err := s.Login(peer, user.Username, initialPassword, testOTP(secret, *now)); err != nil {
		t.Fatal("no refill", err)
	}
}
func TestTOTPSetupPasswordAndAuditRollback(t *testing.T) {
	s, db, now, _, user, current := totpFixture(t)
	ctx := audit.WithActor(context.Background(), user.ID)
	if _, err := s.SetupTOTP(ctx, user.ID, "wrong password"); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatal(err)
	}
	setup, err := s.SetupTOTP(ctx, user.ID, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER reject_totp_audit BEFORE INSERT ON audit_events WHEN NEW.action='user.totp.enable' BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmTOTP(ctx, user.ID, current.Token, testOTP(setup.Secret, *now)); err == nil {
		t.Fatal("audit failure committed")
	}
	status, _ := s.TOTPStatus(ctx, user.ID)
	if status.Enabled || status.RecoveryCodesRemaining != 0 {
		t.Fatal("partial enable")
	}
	var remaining int
	db.QueryRow("SELECT count(*) FROM sessions WHERE user_id=?", user.ID).Scan(&remaining)
	if remaining != 1 {
		t.Fatal("session changed")
	}
}

func TestTOTPLoginRejectsConcurrentStateChanges(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			s, db, now, box, user, current := totpFixture(t)
			otp := ""
			if enabled {
				secret, _ := enableTOTP(t, s, user, current, *now)
				*now = now.Add(30 * time.Second)
				otp = testOTP(secret, *now)
			}
			random := newBlockingReader(91)
			service := auth.NewService(stableDatabase(t, db), auth.Options{Now: func() time.Time { return *now }, SecretBox: box, Random: random})
			result := make(chan error, 1)
			go func() {
				_, err := service.Login(context.Background(), user.Username, initialPassword, otp)
				result <- err
			}()
			<-random.entered
			next := any(nil)
			if !enabled {
				cipher, err := box.Seal("totp:"+user.ID, []byte("12345678901234567890"))
				if err != nil {
					close(random.release)
					t.Fatal(err)
				}
				next = cipher
			}
			_, err := db.Exec(`UPDATE users SET totp_ciphertext=? WHERE id=?`, next, user.ID)
			close(random.release)
			if err != nil {
				t.Fatal(err)
			}
			err = <-result
			if !errors.Is(err, auth.ErrUnauthenticated) && !errors.Is(err, auth.ErrTOTPInvalid) {
				t.Fatal("stale factor state committed", err)
			}
			var count int
			if err = db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, user.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("state conflict created session", count, err)
			}
		})
	}
}

func TestTOTPRevokedSessionCannotConfirmOrChange(t *testing.T) {
	s, db, now, _, user, current := totpFixture(t)
	ctx := context.Background()
	setup, err := s.SetupTOTP(ctx, user.ID, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Logout(ctx, current.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmTOTP(ctx, user.ID, current.Token, testOTP(setup.Secret, *now)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked session confirmed", err)
	}
	current, err = s.Login(ctx, user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	_, codes := enableTOTP(t, s, user, current, *now)
	if err = s.Logout(ctx, current.Token); err != nil {
		t.Fatal(err)
	}
	if err = s.DisableTOTP(ctx, user.ID, current.Token, initialPassword, codes[0]); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked session disabled", err)
	}
	if _, err = s.RegenerateRecoveryCodes(ctx, user.ID, current.Token, initialPassword, codes[0]); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked session regenerated", err)
	}
	status, err := s.TOTPStatus(ctx, user.ID)
	if err != nil || !status.Enabled || status.RecoveryCodesRemaining != 10 {
		t.Fatal("revoked attempt changed state", status, err)
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, user.ID).Scan(&count)
	if count != 0 {
		t.Fatal("revoked session returned")
	}
}

func TestTOTPLoginAcceptsDisplayedSpacing(t *testing.T) {
	s, _, now, _, user, current := totpFixture(t)
	secret, _ := enableTOTP(t, s, user, current, *now)
	*now = now.Add(time.Minute)
	code := testOTP(secret, *now)
	if _, err := s.Login(context.Background(), user.Username, initialPassword, code[:3]+" "+code[3:]); err != nil {
		t.Fatal(err)
	}
}

func TestResetTOTPClearsRecoveryCodes(t *testing.T) {
	s, db, now, _, user, current := totpFixture(t)
	enableTOTP(t, s, user, current, *now)
	ctx := audit.WithSystemActor(context.Background(), "test")
	if err := auth.ResetTOTP(ctx, stableDatabase(t, db), user.Username, *now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM recovery_codes`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("recovery codes=%d err=%v", count, err)
	}
	if err := auth.ResetTOTP(ctx, stableDatabase(t, db), "missing", *now); !errors.Is(err, auth.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestInvalidSecondFactorCountsAsLoginFailure(t *testing.T) {
	s, _, now, _, user, current := totpFixture(t)
	secret, _ := enableTOTP(t, s, user, current, *now)
	wrong := "000000"
	for delta := -30 * time.Second; delta <= 30*time.Second; delta += 30 * time.Second {
		if testOTP(secret, now.Add(delta)) == wrong {
			wrong = "000001"
		}
	}
	// 前 10 次走 TOTP 校验，之后 TOTP 额度耗尽，改走恢复码路径，两条路径都必须计入。
	for i := range 20 {
		otp := wrong
		if i >= 10 {
			otp = "not-a-recovery-code"
		}
		ctx := auth.WithLoginPeer(context.Background(), fmt.Sprintf("192.0.2.%d:1000", i+1))
		if _, err := s.Login(ctx, user.Username, initialPassword, otp); !errors.Is(err, auth.ErrTOTPInvalid) {
			t.Fatal(i, err)
		}
	}
	ctx := auth.WithLoginPeer(context.Background(), "192.0.2.200:1000")
	if _, err := s.Login(ctx, user.Username, initialPassword, ""); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatal(err)
	}
}

func TestTOTPResetAndDisableClearFailureBudget(t *testing.T) {
	s, _, now, _, user, current := totpFixture(t)
	ctx := audit.WithActor(context.Background(), user.ID)
	actor, err := s.CreateUser(ctx, "budget-operator", initialPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	exhaust := func(secret string) {
		t.Helper()
		wrong := "000000"
		if testOTP(secret, *now) == wrong || testOTP(secret, now.Add(30*time.Second)) == wrong || testOTP(secret, now.Add(-30*time.Second)) == wrong {
			wrong = "000001"
		}
		for i := range 10 {
			peer := auth.WithLoginPeer(context.Background(), fmt.Sprintf("192.0.2.%d:1000", i+1))
			if _, err := s.Login(peer, user.Username, initialPassword, wrong); !errors.Is(err, auth.ErrTOTPInvalid) {
				t.Fatal(i, err)
			}
		}
	}
	secret, _ := enableTOTP(t, s, user, current, *now)
	exhaust(secret)
	operator, err := s.Login(ctx, actor.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResetUserTOTP(ctx, actor.ID, operator.Token, user.ID, initialPassword); err != nil {
		t.Fatal(err)
	}
	session, err := s.Login(auth.WithLoginPeer(context.Background(), "192.0.2.100:1000"), user.Username, initialPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	secret, codes := enableTOTP(t, s, user, session, *now)
	exhaust(secret)
	*now = now.Add(time.Minute)
	if err = s.DisableTOTP(ctx, user.ID, session.Token, initialPassword, codes[0]); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	enableTOTP(t, s, user, session, *now)
}
