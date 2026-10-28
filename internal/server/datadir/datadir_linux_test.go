//go:build linux

package datadir_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"orbitbeacon/internal/server/datadir"
)

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateRestoreDoesNotCreateMissingFiles(t *testing.T) {
	for _, missingDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing directory", true: "missing directory"}[missingDirectory], func(t *testing.T) {
			dir := privateDirectory(t)
			if missingDirectory {
				dir = filepath.Join(dir, "missing")
			}
			if err := datadir.ValidateRestore(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "secrets.key")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("validation created a key: %v", err)
			}
			if missingDirectory {
				if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("validation created the directory: %v", err)
				}
			}
		})
	}
}

func TestValidateRestoreDoesNotParseOrReplaceKeyOrDatabase(t *testing.T) {
	for _, key := range [][]byte{[]byte("invalid key"), make([]byte, 32)} {
		dir := privateDirectory(t)
		keyPath := filepath.Join(dir, "secrets.key")
		if err := os.WriteFile(keyPath, key, 0600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"orbitbeacon.db", "metrics.db"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("not SQLite"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := datadir.ValidateRestore(dir); err != nil {
			t.Fatalf("metadata-only validation parsed contents: %v", err)
		}
		if after, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(after, key) {
			t.Fatalf("validation changed invalid key: %v", err)
		}
		if err := datadir.Prepare(dir); err == nil {
			t.Fatal("startup accepted invalid key contents")
		}
		if after, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(after, key) {
			t.Fatalf("startup changed invalid key: %v", err)
		}
	}
}

func TestManagedDatabaseFilesAreValidatedBeforeKeyCreation(t *testing.T) {
	for _, database := range []string{"orbitbeacon.db", "metrics.db"} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			for _, unsafe := range []string{"public", "symlink", "directory", "foreign owner"} {
				t.Run(database+suffix+"/"+unsafe, func(t *testing.T) {
					dir := privateDirectory(t)
					path := filepath.Join(dir, database+suffix)
					switch unsafe {
					case "symlink":
						target := filepath.Join(dir, "target")
						if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(target, path); err != nil {
							t.Fatal(err)
						}
					case "directory":
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					default:
						if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
							t.Fatal(err)
						}
						if unsafe == "public" {
							if err := os.Chmod(path, 0644); err != nil {
								t.Fatal(err)
							}
						} else {
							if err := os.Chown(path, os.Geteuid()+1, -1); err != nil {
								if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
									t.Skip("changing file ownership is not permitted")
								}
								t.Fatal(err)
							}
							t.Cleanup(func() { _ = os.Chown(path, os.Geteuid(), -1) })
						}
					}
					before, err := os.Lstat(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := datadir.ValidateRestore(dir); err == nil {
						t.Fatal("restore validation accepted unsafe managed file")
					}
					if err := datadir.Prepare(dir); err == nil {
						t.Fatal("startup accepted unsafe managed file")
					}
					if _, err := os.Lstat(filepath.Join(dir, "secrets.key")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("key created before rejecting unsafe file: %v", err)
					}
					after, err := os.Lstat(path)
					if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
						t.Fatalf("validation replaced or repaired unsafe file: %v", err)
					}
				})
			}
		}
	}
}

func TestLoadKeyRequiresNonzero32Bytes(t *testing.T) {
	for _, size := range []int{0, 31, 32, 33} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := filepath.Join(privateDirectory(t), "secrets.key")
			key := bytes.Repeat([]byte{7}, size)
			if err := os.WriteFile(path, key, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := datadir.LoadKey(path)
			if size != 32 {
				if err == nil {
					t.Fatal("accepted invalid key size")
				}
				return
			}
			if err != nil || !bytes.Equal(got, key) {
				t.Fatalf("valid key read = %x, %v", got, err)
			}
			clear(got)
		})
	}
	path := filepath.Join(privateDirectory(t), "secrets.key")
	if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := datadir.LoadKey(path); err == nil {
		t.Fatal("accepted all-zero key")
	}
}

func TestLockKeepsInodeAcrossCloseAndReacquire(t *testing.T) {
	dir := privateDirectory(t)
	first, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := first.Stat()
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		lock, err := datadir.Lock(dir)
		if err != nil {
			t.Fatal(err)
		}
		after, err := lock.Stat()
		if err != nil || !os.SameFile(before, after) {
			lock.Close()
			t.Fatalf("lock inode changed: %v", err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(filepath.Join(dir, "server.lock"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("closing lock removed or replaced inode: %v", err)
	}
}
