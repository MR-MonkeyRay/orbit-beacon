package auth

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestPasswordQueueRejectsOverflowAndCancelsWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService(nil, Options{})
		s.gate <- struct{}{} // One password calculation is already running.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.acquire(ctx); !errors.Is(err, context.Canceled) {
					t.Errorf("queued work cancellation=%v", err)
				}
			}()
		}
		synctest.Wait()
		peers, names := maps.Clone(s.loginPeers), maps.Clone(s.loginNames)
		peerOverflow, nameOverflow := s.loginPeerOverflow, s.loginNameOverflow
		started := time.Now()
		if _, err := s.Login(context.Background(), "admin", "a valid length password"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("overflow login admission=%v", err)
		}
		if time.Since(started) != 0 {
			t.Fatal("overflow work was queued instead of immediately rejected")
		}
		if !maps.Equal(peers, s.loginPeers) || !maps.Equal(names, s.loginNames) ||
			peerOverflow != s.loginPeerOverflow || nameOverflow != s.loginNameOverflow || len(s.loginFailures) != 0 || len(s.loginSuccesses) != 0 {
			t.Fatal("gate-rejected login changed limiter state")
		}
		cancel()
		wg.Wait()
		<-s.gate
		cancelled, stop := context.WithCancel(context.Background())
		stop()
		if err := s.acquire(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("already-cancelled work acquired the gate: %v", err)
		}
	})
}

func TestPasswordQueueWaitHasAnInternalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService(nil, Options{})
		s.gate <- struct{}{}
		if err := s.acquire(context.Background()); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("unbounded wait did not return overload: %v", err)
		}
		<-s.gate
		if err := s.acquire(context.Background()); err != nil {
			t.Fatalf("timed-out waiter leaked admission: %v", err)
		}
		<-s.gate
	})
}

func TestLoginAdmissionRefillsAndBoundsIdentityChurn(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	for range 10 {
		if err := s.admitLogin(ctx, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.admitLogin(ctx, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("exhausted computation budget=%v", err)
	}
	now = now.Add(30 * time.Second)
	if err := s.admitLogin(ctx, "admin"); err != nil {
		t.Fatalf("budget did not refill: %v", err)
	}
	if err := s.admitLogin(ctx, "admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("refill allowed an unbounded burst: %v", err)
	}
	for i := range 2048 {
		peer := WithLoginPeer(context.Background(), fmt.Sprintf("198.18.%d.%d:1234", i/256, i%256))
		if err := s.admitLogin(peer, fmt.Sprint(i)); err != nil && !errors.Is(err, ErrRateLimited) {
			t.Fatal(err)
		}
	}
	if len(s.loginPeers) > 1024 || len(s.loginNames) > 1024 {
		t.Fatalf("source churn caused unbounded state: peers=%d identities=%d", len(s.loginPeers), len(s.loginNames))
	}
}

func TestRejectedPeerCannotEvictExhaustedIdentityHistory(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{Now: func() time.Time { return now }})
	ctx := WithLoginPeer(context.Background(), "192.0.2.1:1234")
	for range 10 {
		if err := s.admitLogin(ctx, "target"); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Nanosecond)
	for i := range 22 {
		if err := s.admitLogin(ctx, fmt.Sprintf("admitted-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	identities := len(s.loginNames)
	for i := range 2048 {
		if err := s.admitLogin(ctx, fmt.Sprintf("rejected-%d", i)); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("exhausted source admitted churn: %v", err)
		}
	}
	if len(s.loginNames) != identities {
		t.Errorf("rejected requests rewrote identity history: before=%d after=%d", identities, len(s.loginNames))
	}
	now = now.Add(250 * time.Millisecond) // One peer token, less than one identity token.
	if err := s.admitLogin(ctx, "target"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rejected churn reset the exhausted target budget: %v", err)
	}
}

func TestGateRejectedPasswordChangesDoNotSpendBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService(nil, Options{})
		s.gate <- struct{}{}
		overflow := s.passwordChangeOverflow
		for range 6 {
			if err := s.ChangePassword(context.Background(), "user", "old password", "a valid new password"); !errors.Is(err, ErrRateLimited) {
				t.Fatalf("busy password change=%v", err)
			}
		}
		cancelled, stop := context.WithCancel(context.Background())
		stop()
		if err := s.ChangePassword(cancelled, "user", "old password", "a valid new password"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled password change=%v", err)
		}
		if len(s.passwordChanges) != 0 || s.passwordChangeOverflow != overflow {
			t.Fatal("rejected password changes spent the user's budget")
		}
		<-s.gate
	})
}
