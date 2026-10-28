package auth

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAPITokenLimitersBoundedAndIPv6PeerNormalization(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	for i := range 1100 {
		for range 30 {
			if err := s.admitAPIToken(fmt.Sprint(i)); err != nil && i < 1024 {
				t.Fatal(err)
			}
		}
	}
	if len(s.apiTokenBuckets) > 1024 {
		t.Fatal("unbounded token limiter")
	}
	if err := s.admitAPIToken("new-overflow"); err != ErrRateLimited {
		t.Fatalf("churn reset overflow budget: %v", err)
	}
	a := WithLoginPeer(context.Background(), "[2001:db8:1:2::1]:1234")
	b := WithLoginPeer(context.Background(), "[2001:db8:1:2::2]:4321")
	for range 30 {
		if err := s.checkInvalidAPIPeer(a, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.checkInvalidAPIPeer(b, false); err != ErrRateLimited {
		t.Fatalf("IPv6 /64 bypass: %v", err)
	}
	now = now.Add(time.Second)
	if err := s.checkInvalidAPIPeer(b, true); err != nil {
		t.Fatalf("peer refill: %v", err)
	}
}
