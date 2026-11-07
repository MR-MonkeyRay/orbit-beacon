package auth

import (
	"context"
	"errors"
	"fmt"
	"orbitbeacon/internal/server/storage"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestAPITokenBucketBurstRefillLimitsAndIsolation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	for i := 0; i < 3; i++ {
		if err := s.admitAPIToken("a", 3, s.now()); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(s.admitAPIToken("a", 3, s.now()), ErrRateLimited) {
		t.Fatal("burst exceeded")
	}
	if err := s.admitAPIToken("b", 1, s.now()); err != nil {
		t.Fatal("shared bucket", err)
	}
	now = now.Add(10 * time.Second)
	var limited *APITokenRateLimitError
	if !errors.As(s.admitAPIToken("a", 3, s.now()), &limited) || limited.RetryAfter != 10 {
		t.Fatal("fractional refill", limited)
	}
	now = now.Add(10 * time.Second)
	if err := s.admitAPIToken("a", 3, s.now()); err != nil {
		t.Fatal("refill", err)
	}
	// Raising changes the rate and capacity without granting the difference.
	if !errors.Is(s.admitAPIToken("a", 10, s.now()), ErrRateLimited) {
		t.Fatal("raise granted requests")
	}
	now = now.Add(6 * time.Second)
	if err := s.admitAPIToken("a", 10, s.now()); err != nil {
		t.Fatal("raise did not change the refill rate", err)
	}
	now = now.Add(time.Minute)
	// Clamp even without any elapsed clock time.
	if err := s.admitAPIToken("a", 1, s.now()); err != nil {
		t.Fatal(err)
	}
	if !errors.As(s.admitAPIToken("a", 1, s.now()), &limited) || limited.RetryAfter != 60 {
		t.Fatal("shrink not clamped", limited)
	}
}

func TestAPITokenBucketsConcurrentAndPruned(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{})
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.admitAPIToken("a", 17, now) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 17 {
		t.Fatalf("accepted=%d", accepted.Load())
	}
	for i := 0; i < 100; i++ {
		s.admitAPIToken(fmt.Sprint(i), 1, now.Add(time.Duration(i+1)*time.Minute))
	}
	if len(s.apiTokenBuckets) > maxAPITokens {
		t.Fatalf("unbounded: %d", len(s.apiTokenBuckets))
	}
}

func TestInvalidAPITokenCreatesNoBucket(t *testing.T) {
	s := NewService(nil, Options{})
	if !errors.Is(s.AuthenticateAPIToken(t.Context(), "obt_invalid"), ErrUnauthenticated) || len(s.apiTokenBuckets) != 0 {
		t.Fatal("invalid token bucket")
	}
}

// 查询在 rateMu 外完成，改限额前读到的旧值可能晚于新值到达。
func TestStaleAPITokenLimitDoesNotGrantExtraRequests(t *testing.T) {
	s := NewService(nil, Options{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.admitAPIToken("a", 1, now); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for range 3 {
		if s.admitAPIToken("a", 600, now) == nil {
			accepted++
		}
		for s.admitAPIToken("a", 600, now) == nil {
			accepted++
		}
		_ = s.admitAPIToken("a", 1, now) // stale snapshot arrives after the raise
	}
	if accepted != 0 {
		t.Fatalf("limit changes granted %d requests with no time elapsed", accepted)
	}
}

// 活跃的满桶也不能删除：下一次请求会按更高的限额重建，等于发放差额。
func TestAPITokenPruningKeepsActiveFullBuckets(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{})
	for i := range maxAPITokens {
		s.apiTokenBuckets[fmt.Sprint(i)] = apiTokenBucket{loginBucket: loginBucket{tokens: 1, at: now}, limit: 1}
	}
	s.admitAPIToken("new", 1, now)
	if len(s.apiTokenBuckets) != maxAPITokens+1 {
		t.Fatalf("pruning retained %d active buckets", len(s.apiTokenBuckets))
	}
}

func TestRaisedAPITokenLimitSettlesElapsedTimeAtNewRate(t *testing.T) {
	s := NewService(nil, Options{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.admitAPIToken("a", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := s.admitAPIToken("a", 600, now.Add(time.Second)); err != nil {
		t.Fatal("raised token still waited at the old rate", err)
	}
}

func TestAPITokenRetryAfterRoundsUp(t *testing.T) {
	s := NewService(nil, Options{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var limited *APITokenRateLimitError
	for range 7 {
		if err := s.admitAPIToken("a", 7, now); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.As(s.admitAPIToken("a", 7, now), &limited) || limited.RetryAfter != 9 {
		t.Fatalf("retry after = %+v, want 9 (60/7 rounded up)", limited)
	}
}

func TestAPITokenPruningPreservesDepletedBuckets(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{})
	s.admitAPIToken("depleted", 1, now)
	for i := 0; i < maxAPITokens; i++ {
		s.apiTokenBuckets[fmt.Sprint(i)] = apiTokenBucket{loginBucket: loginBucket{tokens: 2, at: now.Add(-time.Minute)}, limit: 2}
	}
	s.admitAPIToken("active", 1, now)
	if len(s.apiTokenBuckets) != 2 {
		t.Fatalf("pruning retained %d buckets", len(s.apiTokenBuckets))
	}
	if !errors.Is(s.admitAPIToken("depleted", 1, now), ErrRateLimited) {
		t.Fatal("pruning reset depleted bucket")
	}
}

// A cancelled query must return while rateMu is held elsewhere: the lock also guards
// logins, so holding it across the database read would stall them behind a slow query.
func TestAuthenticateAPITokenQueriesOutsideRateLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenMain(t.Context(), filepath.Join(dir, "tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewService(db, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.AuthenticateAPIToken(ctx, "obt_"+strings.Repeat("A", 43)) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication waited for rateMu during its query")
	}
}

func TestEffectiveAPITokenLimitFallback(t *testing.T) {
	for _, value := range []string{"", "bad", "0", "100001", "1.5", " 2"} {
		if got := effectiveAPITokenLimit(value); got != 600 {
			t.Fatalf("fallback %q=%d", value, got)
		}
	}
	for value, want := range map[string]int{"1": 1, "600": 600, "100000": 100000} {
		if got := effectiveAPITokenLimit(value); got != want {
			t.Fatalf("value %q=%d", value, got)
		}
	}
}
