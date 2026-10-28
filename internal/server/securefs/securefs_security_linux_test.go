//go:build linux

package securefs_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"orbitbeacon/internal/server/securefs"
)

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateDirectoryRejectsRenameableAncestors(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mode      os.FileMode
		wantError bool
	}{
		{"private", 0700, false},
		{"root style shared read only", 0755, false},
		{"group writable without sticky", 0770, true},
		{"other writable without sticky", 0777, true},
		{"sticky protects server owned entry", os.ModeSticky | 0777, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ancestor := privateDirectory(t)
			// An intervening private directory must not hide an unsafe ancestor.
			dir := filepath.Join(ancestor, "server-owned", "data")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(ancestor, tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := securefs.ValidateDirectory(dir); (err != nil) != tt.wantError {
				t.Errorf("ValidateDirectory with ancestor mode %s = %v, want error %t", tt.mode, err, tt.wantError)
			}
			if err := securefs.ValidateParents(filepath.Join(dir, "orbitbeacon.db")); (err != nil) != tt.wantError {
				t.Errorf("ValidateParents with ancestor mode %s = %v, want error %t", tt.mode, err, tt.wantError)
			}
		})
	}
}

func TestValidateDirectoryAllowsRootOwnedStickyTmp(t *testing.T) {
	info, err := os.Lstat("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSticky == 0 || info.Mode().Perm()&0002 == 0 {
		t.Skip("requires standard root-owned sticky /tmp")
	}
	dir, err := os.MkdirTemp("/tmp", "orbitbeacon-securefs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := securefs.ValidateDirectory(dir); err != nil {
		t.Fatalf("server-owned 0700 entry under sticky /tmp rejected: %v", err)
	}
	file, err := securefs.OpenFile(filepath.Join(dir, "private"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDirectoryRejectsSymlinksAtEveryDepth(t *testing.T) {
	for _, depth := range []string{"final data directory", "parent", "higher ancestor"} {
		t.Run(depth, func(t *testing.T) {
			root := privateDirectory(t)
			real := filepath.Join(root, "real")
			if err := os.MkdirAll(filepath.Join(real, "nested", "data"), 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			dir := link
			if depth == "parent" {
				dir = filepath.Join(link, "nested")
			} else if depth == "higher ancestor" {
				dir = filepath.Join(link, "nested", "data")
			}
			if err := securefs.ValidateDirectory(dir); err == nil {
				t.Fatal("accepted symlink in data directory path")
			}
			if err := securefs.ValidateParents(filepath.Join(dir, "orbitbeacon.db")); err == nil {
				t.Fatal("accepted symlink ancestor of database")
			}
		})
	}
}

func TestValidateDirectoryRejectsUntrustedOwnersAndNonprivateFinalDirectory(t *testing.T) {
	for _, mode := range []os.FileMode{0750, 0755, 0777, os.ModeSticky | 0700} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := privateDirectory(t)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			if err := securefs.ValidateDirectory(dir); err == nil {
				t.Fatalf("accepted final directory mode %s instead of exact 0700", mode)
			}
		})
	}
	if os.Geteuid() != 0 {
		t.Skip("changing directory ownership requires root")
	}
	for _, kind := range []string{"nonwritable ancestor", "sticky ancestor", "entry below sticky ancestor", "final directory"} {
		t.Run(kind, func(t *testing.T) {
			root := privateDirectory(t)
			ancestor := filepath.Join(root, "ancestor")
			entry := filepath.Join(ancestor, "entry")
			dir := filepath.Join(entry, "data")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			foreign := ancestor
			switch kind {
			case "nonwritable ancestor":
				if err := os.Chmod(ancestor, 0555); err != nil {
					t.Fatal(err)
				}
			case "sticky ancestor", "entry below sticky ancestor":
				if err := os.Chmod(ancestor, os.ModeSticky|0777); err != nil {
					t.Fatal(err)
				}
				if kind == "entry below sticky ancestor" {
					foreign = entry
				}
			case "final directory":
				foreign = dir
			}
			if err := os.Chown(foreign, 65534, -1); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chown(foreign, 0, -1) })
			if err := securefs.ValidateDirectory(dir); err == nil {
				t.Fatalf("accepted foreign-owned %s", kind)
			}
		})
	}
}

func TestOpenRegularAllowsReadableArchiveAndRejectsLinksAndSpecialFiles(t *testing.T) {
	dir := privateDirectory(t)
	path := filepath.Join(dir, "archive")
	if err := os.WriteFile(path, []byte("ciphertext"), 0644); err != nil {
		t.Fatal(err)
	}
	file, err := securefs.OpenRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(privateDirectory(t), "linked")
	if err := os.Symlink(dir, ancestor); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{link, filepath.Join(ancestor, "archive"), fifo, dir, filepath.Join(dir, "missing")} {
		if file, err := securefs.OpenRegular(invalid); err == nil {
			file.Close()
			t.Fatalf("accepted nonregular or linked path %q", invalid)
		}
	}
}
