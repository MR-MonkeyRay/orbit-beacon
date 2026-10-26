//go:build !windows

package filelock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"orbitbeacon/internal/agent/filelock"
)

func TestContendedLockIsCanceledAndNeverAcquiredLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		unlock, err := filelock.Acquire(ctx, path)
		if unlock != nil {
			unlock()
		}
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("contended lock ignored cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled lock wait did not return")
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("canceled waiter retained ownership: %v", err)
	}
	unlock()
}

func TestContendedLockHasFiniteDeadlineWithoutCallerDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.lock")
	unlock, err := filelock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	if acquired, err := filelock.Acquire(context.Background(), path); !errors.Is(err, context.DeadlineExceeded) {
		if acquired != nil {
			acquired()
		}
		t.Fatalf("missing bounded contention deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("lock contention exceeded the documented bound: %s", elapsed)
	}
}
