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

func TestTryAcquireRetainsInodeAndFailsImmediately(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "run.lock")
	unlock, err := filelock.TryAcquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	before, err := os.Stat(path)
	if err != nil || before.Mode().Perm() != 0600 {
		t.Fatalf("unsafe run lock: info=%v err=%v", before, err)
	}
	start := time.Now()
	second, err := filelock.TryAcquire(context.Background(), path)
	if second != nil {
		second()
	}
	if !errors.Is(err, filelock.ErrLocked) || time.Since(start) > time.Second {
		t.Fatalf("contended run lock did not fail immediately: %v", err)
	}
	unlock()
	unlock, err = filelock.TryAcquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("release replaced the stable lock inode: %v", err)
	}
}

func TestTryAcquireRejectsUnsafeStateAndLockFiles(t *testing.T) {
	for _, kind := range []string{"directory symlink", "ancestor symlink", "file symlink", "file mode", "directory mode", "fifo", "hard link", "owner", "directory owner"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "state")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "run.lock")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory symlink", "ancestor symlink":
				link := filepath.Join(root, "link")
				if err := os.Symlink(directory, link); err != nil {
					t.Fatal(err)
				}
				if kind == "ancestor symlink" {
					if err := os.Mkdir(filepath.Join(directory, "child"), 0700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(link, "child", "run.lock")
				} else {
					path = filepath.Join(link, "run.lock")
				}
			case "file symlink":
				link := filepath.Join(directory, "link.lock")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "file mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory mode":
				if err := os.Chmod(directory, 0755); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "hard link":
				if err := os.Link(path, filepath.Join(directory, "other.lock")); err != nil {
					t.Fatal(err)
				}
			case "owner", "directory owner":
				if os.Geteuid() != 0 {
					t.Skip("changing file ownership requires root")
				}
				target := path
				if kind == "directory owner" {
					target = directory
				}
				if err := os.Chown(target, 1, 1); err != nil {
					t.Fatal(err)
				}
			}
			unlock, err := filelock.TryAcquire(context.Background(), path)
			if unlock != nil {
				unlock()
			}
			if err == nil {
				t.Fatal("unsafe run lock was accepted")
			}
		})
	}
}

func TestTryAcquireCanceledDoesNotCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if unlock, err := filelock.TryAcquire(ctx, path); unlock != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled acquisition created lock: %v", err)
	}
}
