package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestValidateFilesystemUsesContainingMountAndRejectsUnsafeStorage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux mountinfo contract")
	}
	for _, tt := range []struct {
		name      string
		mount     string
		path      string
		wantError bool
	}{
		{"local", "ext4 /dev/vda rw", "/data/orbit/db", false},
		{"NFS", "nfs server:/export rw", "/data/orbit/db", true},
		{"NFSv4", "nfs4 server:/export rw", "/data/orbit/db", true},
		{"SMB", "cifs //server/share rw", "/data/orbit/db", true},
		{"legacy SMB", "smb3 //server/share rw", "/data/orbit/db", true},
		{"SSHFS", "fuse.sshfs user@server:/data rw", "/data/orbit/db", true},
		{"Ceph", "ceph server:/ rw", "/data/orbit/db", true},
		{"GlusterFS", "fuse.glusterfs server:/data rw", "/data/orbit/db", true},
		{"readonly superblock", "ext4 /dev/vda ro", "/data/orbit/db", true},
		{"mountpoint prefix is not ancestry", "nfs server:/export rw", "/database/orbit/db", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mountInfo := "20 1 8:1 / / rw,relatime - ext4 /dev/root rw\n" +
				"21 20 0:42 / /data rw,relatime - " + tt.mount + "\n"
			err := storage.ValidateFilesystem(tt.path, strings.NewReader(mountInfo))
			if (err != nil) != tt.wantError {
				t.Errorf("filesystem validation error = %v, want error %t", err, tt.wantError)
			}
		})
	}
	for _, tt := range []struct {
		name      string
		path      string
		mountInfo string
		wantError bool
	}{
		{"readonly bind mount", "/data/db", "20 1 8:1 / / rw - ext4 /dev/root rw\n21 20 8:1 /sub /data ro - ext4 /dev/root rw\n", true},
		{"nested network mount", "/data/nested/db", "20 1 8:1 / / rw - ext4 /dev/root rw\n21 20 0:42 / /data/nested rw - nfs server:/export rw\n", true},
		{"nested local mount supersedes network parent", "/data/local/db", "20 1 8:1 / / rw - ext4 /dev/root rw\n21 20 0:42 / /data rw - nfs server:/export rw\n22 21 8:2 / /data/local rw - ext4 /dev/vdb rw\n", false},
		{"escaped mountpoint", "/data volume/db", "20 1 8:1 / / rw - ext4 /dev/root rw\n21 20 0:42 / /data\\040volume rw - nfs server:/export rw\n", true},
		{"malformed mount source fails closed", "/data/db", "not mountinfo\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := storage.ValidateFilesystem(tt.path, strings.NewReader(tt.mountInfo))
			if (err != nil) != tt.wantError {
				t.Errorf("filesystem validation error = %v, want error %t", err, tt.wantError)
			}
		})
	}
}

func TestOpenCreatesOnlyOwnerAccessibleDatabaseAndSidecars(t *testing.T) {
	dir := privateDirectory(t)
	path := filepath.Join(dir, "orbitbeacon.db")
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("local temporary filesystem rejected: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE private_mode_sentinel (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			t.Errorf("database%s has unsafe mode %s", suffix, info.Mode())
		}
	}
}

func TestOpenRejectsUnsafeExistingDatabaseAndSidecarsBeforeChangingThem(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		for _, kind := range []string{"group readable", "symlink"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				dir := privateDirectory(t)
				path := filepath.Join(dir, "orbitbeacon.db")
				target := path + suffix
				outside := filepath.Join(t.TempDir(), "outside")
				if kind == "symlink" {
					if err := os.WriteFile(outside, nil, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, target); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(target, nil, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(target, 0640); err != nil {
						t.Fatal(err)
					}
				}
				db, err := storage.Open(context.Background(), path)
				if db != nil {
					_ = db.Close()
				}
				if err == nil {
					t.Error("accepted unsafe database component")
				}
				if kind == "symlink" {
					data, err := os.ReadFile(outside)
					if err != nil || len(data) != 0 {
						t.Errorf("followed symlink and changed outside file: %q, %v", data, err)
					}
					if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Errorf("unsafe symlink changed before rejection: info=%v err=%v", info, err)
					}
				} else if info, err := os.Stat(target); err != nil || info.Size() != 0 {
					t.Errorf("unsafe existing component changed before rejection: info=%v err=%v", info, err)
				}
				if suffix != "" {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Errorf("SQLite database created before unsafe sidecar rejection: %v", err)
					}
				}
			})
		}
	}
}
