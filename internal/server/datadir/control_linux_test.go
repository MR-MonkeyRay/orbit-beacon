//go:build linux

package datadir_test

import (
	"orbitbeacon/internal/server/datadir"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageControlAndSidecarsRejectSymlinksBeforeKeyCreation(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		t.Run("control"+suffix, func(t *testing.T) {
			dir, other := privateDirectory(t), privateDirectory(t)
			outside := filepath.Join(other, "untouched")
			if err := os.WriteFile(outside, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "storage-control.db"+suffix)); err != nil {
				t.Fatal(err)
			}
			if err := datadir.ValidateRestore(dir); err == nil {
				t.Fatal("restore accepted symlinked control component")
			}
			if err := datadir.Prepare(dir); err == nil {
				t.Fatal("startup accepted symlinked control component")
			}
			value, err := os.ReadFile(outside)
			if err != nil || string(value) != "preserved" {
				t.Fatal("validation changed foreign file", err)
			}
			if _, err = os.Lstat(filepath.Join(dir, "secrets.key")); !os.IsNotExist(err) {
				t.Fatal("unsafe control created a replacement key", err)
			}
		})
	}
}
