//go:build !windows

package identity_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"orbitbeacon/internal/agent/identity"
)

// Done is observed when the contended lock starts watching cancellation. This
// handshake avoids sleeps and proves Current is read while the operation waits.
type observedLockContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedLockContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestIdentityLockWaitCancellationKeepsCurrentReadableAndStateUnchanged(t *testing.T) {
	ticket := [32]byte{42}
	operations := []struct {
		name string
		run  func(context.Context, *identity.Store) error
	}{
		{"reload", func(ctx context.Context, store *identity.Store) error {
			_, err := store.Reload(ctx)
			return err
		}},
		{"begin-run", func(ctx context.Context, store *identity.Store) error {
			_, err := store.BeginRun(ctx)
			return err
		}},
		{"confirm", func(ctx context.Context, store *identity.Store) error {
			return store.ConfirmEnrollment(ctx, store.Current().NodeToken, "node")
		}},
		{"lookup", func(ctx context.Context, store *identity.Store) error {
			_, _, _, err := store.LookupEnrollment(ctx, ticket)
			return err
		}},
		{"stage", func(ctx context.Context, store *identity.Store) error {
			_, _, err := store.StageEnrollment(ctx, ticket, true)
			return err
		}},
		{"remember", func(ctx context.Context, store *identity.Store) error {
			_, _, err := store.RememberEnrollment(ctx, ticket, store.Current().NodeToken)
			return err
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "identity.json")
			store, err := identity.Open(path, identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			initial := store.Current()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			holder, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &observedLockContext{Context: base, waiting: make(chan struct{})}
			result := make(chan error, 1)
			go func() { result <- operation.run(ctx, store) }()
			select {
			case <-ctx.waiting:
			case err := <-result:
				t.Fatalf("operation returned instead of waiting for the held sidecar: %v", err)
			case <-time.After(time.Second):
				t.Fatal("operation did not begin a cancellable lock wait")
			}
			current := make(chan identity.Identity, 1)
			go func() { current <- store.Current() }()
			select {
			case got := <-current:
				if got != initial {
					t.Fatal("lock waiter changed the in-memory snapshot")
				}
			case <-time.After(time.Second):
				t.Fatal("Current blocked behind an external lock holder")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lock wait cancellation returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled operation did not return promptly")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || store.Current() != initial {
				t.Fatalf("canceled operation changed identity or enrollment journal: %v", err)
			}
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			probe, err := os.OpenFile(path+".lock", os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatalf("canceled waiter acquired the released sidecar: %v", err)
			}
			if err := unix.Flock(int(probe.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			run, err := store.BeginRun(context.Background())
			if err != nil || run.RunGeneration != initial.RunGeneration+1 || run.PendingNodeToken != "" || run.NodeID != initial.NodeID {
				t.Fatalf("canceled waiter later changed durable identity: %+v, %v", run, err)
			}
			if _, _, exists, err := store.LookupEnrollment(context.Background(), ticket); err != nil || exists {
				t.Fatalf("canceled waiter later staged a candidate: exists=%v err=%v", exists, err)
			}
		})
	}
}
