package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestOpenSyncsParentsOfNewIdentityDirectory(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "a", "state")
	path := filepath.Join(stateDir, "identity.json")
	calls := map[string]int{}
	syncDirectory := func(directory string) error {
		calls[directory]++
		if directory == root {
			return syscall.EIO
		}
		return nil
	}
	store, err := Open(context.Background(), path, Options{syncDirectory: syncDirectory})
	if !errors.Is(err, syscall.EIO) || store != nil {
		t.Fatalf("new state directory was published without syncing its parent: store=%v err=%v", store, err)
	}
	if calls[stateDir] == 0 || calls[root] == 0 {
		t.Fatalf("directory sync sequence omitted leaf or parent: %+v", calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("identity file was not atomically created before parent sync failure: %v", err)
	}
	store, err = Open(context.Background(), path, Options{syncDirectory: syncDirectory})
	if !errors.Is(err, syscall.EIO) || store != nil || calls[root] < 2 {
		t.Fatalf("second open bypassed the unsynced parent boundary: store=%v err=%v calls=%v", store, err, calls)
	}
	store, err = Open(context.Background(), path, Options{syncDirectory: func(string) error { return nil }})
	if err != nil || store == nil {
		t.Fatalf("parent-sync recovery did not publish the existing identity: store=%v err=%v", store, err)
	}
}

func TestOpenRelativePathRechecksAbsoluteAncestorChain(t *testing.T) {
	root := t.TempDir()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(working) })
	path := filepath.Join("a", "state", "identity.json")
	calls := 0
	syncDirectory := func(directory string) error {
		if directory == root {
			calls++
			return syscall.EIO
		}
		return nil
	}
	if store, err := Open(context.Background(), path, Options{syncDirectory: syncDirectory}); !errors.Is(err, syscall.EIO) || store != nil {
		t.Fatalf("relative first open bypassed absolute ancestor failure: store=%v err=%v", store, err)
	}
	if store, err := Open(context.Background(), path, Options{syncDirectory: syncDirectory}); !errors.Is(err, syscall.EIO) || store != nil || calls < 2 {
		t.Fatalf("relative retry bypassed absolute ancestor failure: store=%v err=%v calls=%d", store, err, calls)
	}
	if store, err := Open(context.Background(), path, Options{syncDirectory: func(string) error { return nil }}); err != nil || store == nil {
		t.Fatalf("relative path did not recover after ancestor sync: store=%v err=%v", store, err)
	}
}

func TestOpenFollowsDirectorySymlinkAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory symlink behavior differs on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "state", "identity.json")
	if store, err := Open(context.Background(), path, Options{syncDirectory: func(string) error { return nil }}); err != nil || store == nil {
		t.Fatalf("directory symlink ancestor rejected: store=%v err=%v", store, err)
	}
	if _, err := os.Stat(filepath.Join(target, "state", "identity.json")); err != nil {
		t.Fatalf("identity was not created through directory symlink ancestor: %v", err)
	}
}
