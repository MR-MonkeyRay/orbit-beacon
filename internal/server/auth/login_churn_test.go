package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"testing"
	"testing/synctest"
	"time"
)

func TestDepletedLoginBudgetsSurviveAdmittedKeyChurn(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	pair := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	source := WithLoginPeer(context.Background(), "192.0.2.2:1234")
	for range 10 {
		if err := s.admitLogin(pair, "target-pair"); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 32 {
		if err := s.admitLogin(source, fmt.Sprintf("source-name-%d", i%4)); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		s.recordLoginFailure(context.Background(), "target-name")
	}
	now = now.Add(time.Nanosecond) // Make the protected history strictly oldest.
	for i := range 2048 {
		ctx := WithLoginPeer(context.Background(), fmt.Sprintf("198.18.%d.%d:1234", i/256, i%256))
		name := fmt.Sprintf("churn-%d", i)
		if err := s.admitLogin(ctx, name); err != nil && !errors.Is(err, ErrRateLimited) {
			t.Fatal(err)
		}
		s.recordLoginFailure(ctx, name)
	}
	if len(s.loginNames) != 1024 || len(s.loginPeers) != 1024 || len(s.loginFailures) != 1024 {
		t.Fatalf("unbounded maps: pairs=%d sources=%d names=%d", len(s.loginNames), len(s.loginPeers), len(s.loginFailures))
	}
	pairKey := loginIdentityKey{peer: loginRatePeer(pair), name: sha256.Sum256([]byte("target-pair"))}
	if bucket, ok := s.loginNames[pairKey]; !ok || bucket.tokens != 0 {
		t.Fatalf("depleted pair lost: %+v, present=%t", bucket, ok)
	}
	if bucket, ok := s.loginPeers[loginRatePeer(source)]; !ok || bucket.tokens != 0 {
		t.Fatalf("depleted source lost: %+v, present=%t", bucket, ok)
	}
	if bucket, ok := s.loginFailures[sha256.Sum256([]byte("target-name"))]; !ok || bucket.tokens != 0 {
		t.Fatalf("depleted username lost: %+v, present=%t", bucket, ok)
	}
	for _, attempt := range []struct {
		ctx  context.Context
		name string
	}{{pair, "target-pair"}, {source, "fresh-name"}, {WithLoginPeer(context.Background(), "203.0.113.1:1234"), "target-name"}} {
		if err := s.admitLogin(attempt.ctx, attempt.name); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("churn reset %q: %v", attempt.name, err)
		}
	}
}

func TestLoginBucketOverflowIsSharedAndReclaimsOnlyFullBudgets(t *testing.T) {
	for _, budget := range []struct {
		name          string
		burst, refill float64
	}{{"pair", 10, 1.0 / 30}, {"source", 32, 4}, {"username failure", 20, 1.0 / 60}, {"password change", 5, 1.0 / 60}} {
		t.Run(budget.name, func(t *testing.T) {
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			buckets := make(map[int]loginBucket)
			for i := range 1024 {
				buckets[i] = loginBucket{tokens: 0, at: now}
			}
			overflow := loginBucket{tokens: budget.burst, at: now}
			for i := range int(budget.burst) {
				key := 1024 + i
				bucket := loginBucketFor(buckets, key, overflow, now, budget.burst, budget.refill)
				if bucket.tokens != budget.burst-float64(i) {
					t.Fatalf("new key reset shared budget: %+v", bucket)
				}
				bucket.tokens--
				storeLoginBucket(buckets, key, &overflow, bucket, now, budget.burst, budget.refill)
			}
			if got := loginBucketFor(buckets, 2048, overflow, now, budget.burst, budget.refill); got.tokens != 0 {
				t.Fatalf("overflow newcomer regained a burst: %+v", got)
			}
			if len(buckets) != 1024 {
				t.Fatalf("overflow allocated new keys: %d", len(buckets))
			}
			// Partial refills still contain history; only a full refill is disposable.
			now = now.Add(time.Duration(budget.burst / budget.refill / 2 * float64(time.Second)))
			if _, ok := reclaimableLoginBucket(buckets, now, budget.burst, budget.refill); ok {
				t.Fatal("partially refilled history was reclaimable")
			}
			now = now.Add(24 * time.Hour)
			bucket := loginBucketFor(buckets, 2049, overflow, now, budget.burst, budget.refill)
			if bucket.tokens != budget.burst {
				t.Fatalf("full refill did not admit newcomer: %+v", bucket)
			}
			bucket.tokens--
			storeLoginBucket(buckets, 2049, &overflow, bucket, now, budget.burst, budget.refill)
			if _, ok := buckets[2049]; !ok || len(buckets) != 1024 {
				t.Fatal("fully refilled entry was not reclaimed")
			}
		})
	}
}

func TestLoginSuccessChurnPreservesTrustUntilExpiry(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	target := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	s.recordLoginSuccess(target, "admin")
	created := now
	now = now.Add(time.Nanosecond)
	for i := range 2048 {
		ctx := WithLoginPeer(context.Background(), fmt.Sprintf("198.18.%d.%d:1234", i/256, i%256))
		s.recordLoginSuccess(ctx, fmt.Sprintf("user-%d", i))
	}
	for range 20 {
		s.recordLoginFailure(context.Background(), "admin")
	}
	if err := s.checkLoginUsername(target, "admin"); err != nil {
		t.Fatalf("churn evicted unexpired trusted source: %v", err)
	}
	if len(s.loginSuccesses) != 1024 {
		t.Fatalf("unbounded success records: %d", len(s.loginSuccesses))
	}
	before := maps.Clone(s.loginSuccesses)
	now = created.Add(24*time.Hour - time.Nanosecond)
	newKey := loginIdentityKey{name: sha256.Sum256([]byte("new"))}
	storeLoginSuccess(s.loginSuccesses, newKey, now)
	if !maps.Equal(before, s.loginSuccesses) {
		t.Fatal("success history reclaimed before expiry")
	}
	now = now.Add(time.Nanosecond)
	storeLoginSuccess(s.loginSuccesses, newKey, now)
	if _, ok := s.loginSuccesses[newKey]; !ok || len(s.loginSuccesses) != 1024 {
		t.Fatal("expired success record was not reclaimed")
	}
}

func TestGateTimeoutAndCancellationPreserveExistingLoginState(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := NewService(nil, Options{})
				ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
				if err := s.admitLogin(ctx, "admin"); err != nil {
					t.Fatal(err)
				}
				peers, names := maps.Clone(s.loginPeers), maps.Clone(s.loginNames)
				s.gate <- struct{}{}
				defer func() { <-s.gate }()
				ctx, stop := context.WithCancel(ctx)
				defer stop()
				if cancel {
					go func() { time.Sleep(100 * time.Millisecond); stop() }()
				}
				_, err := s.Login(ctx, "admin", "valid password")
				want := error(ErrRateLimited)
				if cancel {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("gate rejection=%v, want %v", err, want)
				}
				if !maps.Equal(peers, s.loginPeers) || !maps.Equal(names, s.loginNames) {
					t.Fatal("gate rejection changed existing login buckets")
				}
			})
		})
	}
}

func TestQueuedLoginRechecksBothBudgetsBeforeCharging(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService(nil, Options{})
		ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
		for range 9 {
			if err := s.admitLogin(ctx, "admin"); err != nil {
				t.Fatal(err)
			}
		}
		s.gate <- struct{}{}
		result := make(chan error, 1)
		go func() {
			_, err := s.Login(ctx, "admin", "valid password")
			result <- err
		}()
		synctest.Wait()
		if len(s.waiters) != 1 {
			t.Fatal("eligible login did not queue")
		}
		if err := s.admitLogin(ctx, "admin"); err != nil {
			t.Fatal(err)
		}
		peers, names := maps.Clone(s.loginPeers), maps.Clone(s.loginNames)
		<-s.gate
		if err := <-result; !errors.Is(err, ErrRateLimited) {
			t.Fatalf("queued login bypassed exhausted pair: %v", err)
		}
		if !maps.Equal(peers, s.loginPeers) || !maps.Equal(names, s.loginNames) {
			t.Fatal("rejected queued login charged one of its budgets")
		}
	})
}
