package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/secretbox"
)

func TestTOTPRFC6238AndWindow(t *testing.T) {
	secret := []byte("12345678901234567890")
	encoded := totpEncoding.EncodeToString(secret)
	decoded, err := totpEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != string(secret) || strings.Contains(encoded, "=") {
		t.Fatal("base32 round trip")
	}
	for _, tc := range []struct {
		at   int64
		code string
	}{{59, "287082"}, {1111111109, "081804"}, {1234567890, "005924"}} {
		if got := totpCode(secret, tc.at/30); got != tc.code {
			t.Errorf("T=%d got %s", tc.at, got)
		}
	}
	now := time.Unix(1234567890, 0)
	for delta := int64(-2); delta <= 2; delta++ {
		step := now.Unix()/30 + delta
		got, ok := matchTOTP(secret, totpCode(secret, step), now, 0)
		if ok != (delta >= -1 && delta <= 1) || ok && got != step {
			t.Errorf("delta=%d step=%d ok=%t", delta, got, ok)
		}
	}
	if _, ok := matchTOTP(secret, totpCode(secret, now.Unix()/30), now, now.Unix()/30); ok {
		t.Fatal("replay accepted")
	}
}
func TestTOTPFailureBucketIsIndependentAndBounded(t *testing.T) {
	now := time.Unix(1234567890, 0)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	s.recordLoginSuccess(ctx, "admin")
	for range 10 {
		if !s.totpAllowed("admin") {
			t.Fatal("early exhaustion")
		}
		s.secondFactorFailure("admin", "totp")
	}
	if s.totpAllowed("admin") {
		t.Fatal("success trust bypassed TOTP budget")
	}
	now = now.Add(299 * time.Second)
	if s.totpAllowed("admin") {
		t.Fatal("early refill")
	}
	now = now.Add(time.Second)
	if !s.totpAllowed("admin") {
		t.Fatal("missing refill")
	}
	for i := range 1200 {
		for range 10 {
			s.secondFactorFailure(fmt.Sprint(i), "totp")
		}
	}
	if len(s.totpFailures) > 1024 || s.totpAllowed("overflow") {
		t.Fatal("unbounded or reset overflow bucket")
	}
}

func TestTOTPRequiredDoesNotChargeFailureBudget(t *testing.T) {
	box, err := secretbox.New(make([]byte, 32), "orbitbeacon account secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(nil, Options{SecretBox: box})
	for range 100 {
		if _, err = s.verifySecondFactor("admin", totpState{cipher: []byte{1}}, ""); !errors.Is(err, ErrTOTPRequired) {
			t.Fatal(err)
		}
	}
	if len(s.totpFailures) != 0 || len(s.loginFailures) != 0 || !s.totpAllowed("admin") {
		t.Fatal("required charged budget")
	}
	if _, err = s.verifySecondFactor("admin", totpState{}, "arbitrary-otp"); err != nil {
		t.Fatal("disabled did not ignore otp")
	}
	for range 10 {
		s.secondFactorFailure("admin", "totp")
	}
	factor, err := s.verifySecondFactor("admin", totpState{cipher: []byte{1}}, "abcde-fghij-klmno-pqrst")
	if err != nil || factor.kind != "recovery" {
		t.Fatal("recovery locked by TOTP bucket", err)
	}
}
func TestTOTPRecoveryEntropyNormalizationAndURI(t *testing.T) {
	s := NewService(nil, Options{Random: bytes.NewReader(make([]byte, 130))})
	codes, err := s.newRecoveryCodes()
	if err != nil || len(codes) != 10 {
		t.Fatal(err)
	}
	for _, code := range codes {
		if code != "aaaaa-aaaaa-aaaaa-aaaaa" {
			t.Fatal("unexpected zero entropy encoding")
		}
	}
	s.random = bytes.NewReader(make([]byte, 129))
	if _, err = s.newRecoveryCodes(); !errors.Is(err, ErrTOTPUnavailable) {
		t.Fatal("entropy failure leaked", err)
	}
	if recoveryHash("ABCDE-FGHIJ-KLMNO-PQRST") != recoveryHash("abcdefghijklmnopqrst") {
		t.Fatal("normalization")
	}
	username := "admin/+?&#% 中文"
	parsed, err := url.Parse(totpURI(username, "SECRET"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/OrbitBeacon:"+username || parsed.Query().Get("secret") != "SECRET" || parsed.Query().Get("issuer") != "OrbitBeacon" || parsed.Fragment != "" {
		t.Fatal("uri encoding")
	}
	if !strings.Contains(parsed.EscapedPath(), "%2F") {
		t.Fatal("username slash became path separator")
	}
	s.random = io.LimitReader(bytes.NewReader([]byte{1}), 0)
	if _, err = s.newRecoveryCodes(); !errors.Is(err, ErrTOTPUnavailable) {
		t.Fatal(err)
	}
}

func TestRecoveryFailuresDoNotChargeTOTPBudget(t *testing.T) {
	s := NewService(nil, Options{Now: func() time.Time { return time.Unix(1000, 0) }})
	for range 20 {
		s.secondFactorFailure("user", "recovery")
	}
	if !s.totpAllowed("user") {
		t.Fatal("recovery failure charged the TOTP budget")
	}
}

func TestNormalizeOTPRemovesDisplaySpacing(t *testing.T) {
	for _, input := range []string{"123 456", " 123456 ", "123\t456"} {
		if got := normalizeOTP(input); got != "123456" {
			t.Fatalf("%q normalized to %q", input, got)
		}
	}
}
