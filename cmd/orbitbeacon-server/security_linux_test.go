//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPrepareDataDirCreatesPrivateDirectoryAndPersistentKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new-data")
	if err := prepareDataDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Errorf("data directory mode = %s, want private directory", info.Mode())
	}
	keyPath := filepath.Join(dir, "secrets.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 || bytes.Equal(key, make([]byte, 32)) {
		t.Error("preparation did not generate a nonzero 32-byte secrets key")
	}
	info, err = os.Lstat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		t.Errorf("unsafe secrets key metadata: %+v", info)
	}
	if err := prepareDataDir(dir); err != nil {
		t.Fatalf("valid existing installation rejected: %v", err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(key, after) {
		t.Errorf("preparation replaced persistent encryption key: %v", err)
	}
}

func TestPrepareDataDirRejectsSymlinksAndInsecureModes(t *testing.T) {
	for _, kind := range []string{"symlink", "group readable", "group writable", "world accessible", "regular file"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "data")
			switch kind {
			case "symlink":
				outside := t.TempDir()
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			case "regular file":
				if err := os.WriteFile(dir, nil, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0750)
				if kind == "group writable" {
					mode = 0720
				} else if kind == "world accessible" {
					mode = 0701
				}
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareDataDir(dir); err == nil {
				t.Error("accepted insecure data directory")
			}
			if _, err := os.Lstat(filepath.Join(dir, "secrets.key")); !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				t.Errorf("created key before rejecting directory: %v", err)
			}
		})
	}
}

func TestPrepareDataDirRejectsUnsafeExistingSecretsKey(t *testing.T) {
	for _, kind := range []string{"symlink", "group readable", "wrong size"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "secrets.key")
			data := bytes.Repeat([]byte{7}, 32)
			if kind == "wrong size" {
				data = data[:31]
			}
			if kind == "symlink" {
				outside := filepath.Join(t.TempDir(), "key")
				if err := os.WriteFile(outside, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "group readable" {
					if err := os.Chmod(path, 0640); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := prepareDataDir(dir); err == nil {
				t.Error("accepted unsafe existing secrets key")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, data) {
				t.Errorf("rejected key changed: %v", err)
			}
		})
	}
}

func TestPrepareDataDirRejectsForeignOwnedDirectoryAndKey(t *testing.T) {
	for _, component := range []string{"directory", "key"} {
		t.Run(component, func(t *testing.T) {
			dir := t.TempDir()
			target := dir
			if component == "key" {
				target = filepath.Join(dir, "secrets.key")
				if err := os.WriteFile(target, bytes.Repeat([]byte{7}, 32), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chown(target, os.Geteuid()+1, -1); err != nil {
				if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
					t.Skip("changing file ownership is not permitted; no root required")
				}
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chown(target, os.Geteuid(), -1) })
			if err := prepareDataDir(dir); err == nil {
				t.Error("accepted foreign-owned installation component")
			}
		})
	}
}
