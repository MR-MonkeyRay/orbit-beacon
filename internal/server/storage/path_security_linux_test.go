//go:build linux

package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func privateStorageDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenRejectsNonstickyWritableAncestorsBeforeSQLiteCreatesFiles(t *testing.T) {
	for _, mode := range []os.FileMode{0770, 0777} {
		t.Run(mode.String(), func(t *testing.T) {
			ancestor := privateStorageDirectory(t)
			dir := filepath.Join(ancestor, "server-owned", "data")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(ancestor, mode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "orbitbeacon.db")
			db, err := storage.Open(context.Background(), path)
			if db != nil {
				_ = db.Close()
			}
			if err == nil {
				t.Error("accepted a pathname whose private data directory can be renamed by another principal")
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
					t.Errorf("database%s touched before unsafe ancestor rejection: %v", suffix, err)
				}
			}
		})
	}
}

func TestOpenAllowsPrivateDataDirectoryUnderRootOwnedStickyTmp(t *testing.T) {
	info, err := os.Lstat("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSticky == 0 || info.Mode().Perm()&0002 == 0 {
		t.Skip("requires standard root-owned sticky /tmp")
	}
	dir, err := os.MkdirTemp("/tmp", "orbitbeacon-sqlite-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "orbitbeacon.db")
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("server-owned 0700 entry below standard sticky /tmp rejected: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE sticky_tmp_sentinel (value TEXT); INSERT INTO sticky_tmp_sentinel VALUES ('private')"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint64(owner.Uid) != uint64(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			t.Errorf("database%s is not an effective-user-owned private regular file: %v", suffix, info)
		}
	}
}

func TestOpenRejectsSymlinkDataDirectoryAndAncestors(t *testing.T) {
	for _, depth := range []string{"final data directory", "ancestor"} {
		t.Run(depth, func(t *testing.T) {
			root := privateStorageDirectory(t)
			real := filepath.Join(root, "real")
			if err := os.MkdirAll(filepath.Join(real, "data"), 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			dir, outside := link, real
			if depth == "ancestor" {
				dir, outside = filepath.Join(link, "data"), filepath.Join(real, "data")
			}
			db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
			if db != nil {
				_ = db.Close()
			}
			if err == nil {
				t.Error("accepted symlink in database directory path")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "data" || depth != "final data directory" {
					t.Errorf("SQLite escaped into symlink target and created %q", entry.Name())
				}
			}
		})
	}
}
