//go:build !windows

package identity_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"orbitbeacon/internal/agent/identity"
)

func TestOpenWaitsForLockedAtomicPublication(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "first-creation"
		if existing {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "identity.json")
			published := identity.Identity{InstallationID: "first-writer", NodeToken: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
			if existing {
				store, err := identity.Open(t.Context(), path, identity.Options{})
				if err != nil {
					t.Fatal(err)
				}
				published = store.Current()
				published.RunGeneration = 1
				published.SessionID = "writer-run"
			}
			holder := holdOpenSidecar(t, path)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &observedLockContext{Context: base, waiting: make(chan struct{})}
			type outcome struct {
				store *identity.Store
				err   error
			}
			result := make(chan outcome, 1)
			go func() {
				store, err := identity.Open(ctx, path, identity.Options{})
				result <- outcome{store, err}
			}()
			select {
			case <-ctx.waiting:
			case got := <-result:
				t.Fatalf("Open bypassed an in-progress atomic publication: err=%v", got.err)
			case <-time.After(time.Second):
				t.Fatal("Open did not begin its cancellable sidecar wait")
			}
			data, err := json.Marshal(published)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.CreateTemp(directory, ".writer-*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(file.Name())
			if _, err := file.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(file.Name(), path); err != nil {
				t.Fatal(err)
			}
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-result:
				if got.err != nil || got.store.Current() != published {
					t.Fatalf("Open did not load the completed publication: %v", got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("Open did not resume after publication")
			}
		})
	}
}

func TestOpenCancellationDoesNotCreateAnIdentityAfterUnlock(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "identity.json")
	holder := holdOpenSidecar(t, path)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &observedLockContext{Context: base, waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := identity.Open(ctx, path, identity.Options{})
		result <- err
	}()
	select {
	case <-ctx.waiting:
	case err := <-result:
		t.Fatalf("Open created state instead of waiting: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Open did not observe cancellation")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Open returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Open cancellation did not return promptly")
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled Open created identity state: %v", err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("canceled opener acquired the lock later: %v", err)
	}
}

func holdOpenSidecar(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return file
}
