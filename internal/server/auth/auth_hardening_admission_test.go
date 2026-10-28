package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLoginAdmissionAggregatesIPv6By64(t *testing.T) {
	s := NewService(nil, Options{})
	first := WithLoginPeer(context.Background(), "[2001:db8:1:2::1]:1234")
	second := WithLoginPeer(context.Background(), "[2001:db8:1:2::ffff]:5678")
	for range 9 {
		if err := s.admitLogin(first, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.admitLogin(second, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.admitLogin(second, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("same /64 did not share identity budget: %v", err)
	}
	if len(s.loginPeers) != 1 || len(s.loginNames) != 1 {
		t.Fatalf("same /64 created distinct keys: peers=%d identities=%d", len(s.loginPeers), len(s.loginNames))
	}
}

func TestLoginAdmissionKeepsIPv4PerAddress(t *testing.T) {
	s := NewService(nil, Options{})
	first := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	second := WithLoginPeer(context.Background(), "192.0.2.2:1234")
	for range 10 {
		if err := s.admitLogin(first, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.admitLogin(second, "admin"); err != nil {
		t.Fatalf("distinct IPv4 address shared identity budget: %v", err)
	}
}

func TestLoginIdentityRefillsOnePerThirtySeconds(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	for range 10 {
		if err := s.admitLogin(ctx, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(29 * time.Second)
	if err := s.admitLogin(ctx, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("identity refilled early: %v", err)
	}
	now = now.Add(time.Second)
	if err := s.admitLogin(ctx, "admin"); err != nil {
		t.Fatalf("identity did not refill after 30 seconds: %v", err)
	}
}

func TestLoginUsernameFailuresOnlyAndRejectedAdmissionDoesNotCharge(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	name := sha256.Sum256([]byte("admin"))
	key := loginIdentityKey{peer: loginRatePeer(ctx), name: name}
	for range 20 {
		if err := s.checkLoginUsername(ctx, "admin"); err != nil {
			t.Fatalf("eligibility check charged failure budget: %v", err)
		}
		s.recordLoginFailure(ctx, "admin")
	}
	if err := s.admitLogin(ctx, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("exhausted username admitted: %v", err)
	}
	if _, ok := s.loginNames[key]; ok {
		t.Fatal("username rejection charged identity budget")
	}
	if _, ok := s.loginPeers[loginRatePeer(ctx)]; ok {
		t.Fatal("username rejection charged source budget")
	}
	now = now.Add(60 * time.Second)
	if err := s.checkLoginUsername(ctx, "admin"); err != nil {
		t.Fatalf("username failure budget did not refill: %v", err)
	}
}

func TestLoginUsernameTrustedSourceExemptionExpires(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	trusted := WithLoginPeer(context.Background(), "[2001:db8:1:2::1]:1234")
	attacker := WithLoginPeer(context.Background(), "[2001:db8:9:9::1]:1234")
	s.recordLoginSuccess(trusted, "admin")
	for range 20 {
		s.recordLoginFailure(attacker, "admin")
	}
	if err := s.checkLoginUsername(attacker, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("untrusted source bypassed exhausted username: %v", err)
	}
	if err := s.checkLoginUsername(trusted, "admin"); err != nil {
		t.Fatalf("trusted source was locked out: %v", err)
	}
	now = now.Add(24 * time.Hour)
	for range 20 {
		s.recordLoginFailure(attacker, "admin")
	}
	if err := s.checkLoginUsername(trusted, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expired trusted source bypassed username limit: %v", err)
	}
}

func TestLoginSuccessResetsUsernameFailures(t *testing.T) {
	s := NewService(nil, Options{})
	ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	for range 20 {
		s.recordLoginFailure(ctx, "admin")
	}
	s.recordLoginSuccess(ctx, "admin")
	if err := s.checkLoginUsername(context.Background(), "admin"); err != nil {
		t.Fatalf("success did not reset username failures: %v", err)
	}
	if len(s.loginFailures) != 0 {
		t.Fatalf("success retained failure bucket: %d", len(s.loginFailures))
	}
}

func TestInvalidPeerNeverGetsTrustedExemption(t *testing.T) {
	s := NewService(nil, Options{})
	ctx := context.Background()
	s.recordLoginSuccess(ctx, "admin")
	for range 20 {
		s.recordLoginFailure(ctx, "admin")
	}
	if err := s.checkLoginUsername(ctx, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("invalid peer gained trusted exemption: %v", err)
	}
	if len(s.loginSuccesses) != 0 {
		t.Fatalf("invalid peer was stored as trusted: %d", len(s.loginSuccesses))
	}
}

func TestLoginAdmissionStateIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	for i := range 1100 {
		username := fmt.Sprintf("user-%d", i)
		s.recordLoginFailure(context.Background(), username)
		now = now.Add(time.Nanosecond)
	}
	if len(s.loginFailures) != 1024 {
		t.Fatalf("failure state size=%d, want 1024", len(s.loginFailures))
	}
	if _, ok := s.loginFailures[sha256.Sum256([]byte("user-0"))]; !ok {
		t.Fatal("failure churn evicted depleted history")
	}
	for i := range 1100 {
		username := fmt.Sprintf("user-%d", i)
		ctx := WithLoginPeer(context.Background(), fmt.Sprintf("[2001:db8:%x::1]:1234", i))
		s.recordLoginSuccess(ctx, username)
		now = now.Add(time.Nanosecond)
	}
	if len(s.loginSuccesses) != 1024 {
		t.Fatalf("trusted-source state size=%d, want 1024", len(s.loginSuccesses))
	}
	for i := range 1100 {
		if err := s.admitPasswordChange(fmt.Sprintf("user-%d", i)); err != nil && !errors.Is(err, ErrRateLimited) {
			t.Fatal(err)
		}
		now = now.Add(time.Nanosecond)
	}
	if len(s.passwordChanges) != 1024 {
		t.Fatalf("password-change state size=%d, want 1024", len(s.passwordChanges))
	}
}

func TestPasswordChangeAdmissionRate(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	for range 5 {
		if err := s.admitPasswordChange("user-id"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.admitPasswordChange("user-id"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("sixth password change admitted: %v", err)
	}
	now = now.Add(59 * time.Second)
	if err := s.admitPasswordChange("user-id"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("password change refilled early: %v", err)
	}
	now = now.Add(time.Second)
	if err := s.admitPasswordChange("user-id"); err != nil {
		t.Fatalf("password change did not refill: %v", err)
	}
}
