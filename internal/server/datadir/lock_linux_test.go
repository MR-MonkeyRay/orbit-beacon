//go:build linux

package datadir_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/datadir"
)

func TestDataDirectoryLockIsExclusiveAndReleasesOnClose(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "data")
	first, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := datadir.Lock(dir); !errors.Is(err, datadir.ErrInUse) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second server lock=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := datadir.Lock(dir)
	if err != nil {
		t.Fatalf("released server lock could not be reacquired: %v", err)
	}
	defer third.Close()
	if info, err := os.Stat(filepath.Join(dir, "server.lock")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("lock file permissions: %v %v", info, err)
	}
}

func TestDataDirectoryLockRejectsSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "server.lock")); err != nil {
		t.Fatal(err)
	}
	if file, err := datadir.Lock(dir); err == nil {
		file.Close()
		t.Fatal("lock followed a symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("unsafe lock changed its target")
	}
}
