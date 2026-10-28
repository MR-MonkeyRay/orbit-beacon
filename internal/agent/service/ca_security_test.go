package service

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"orbitbeacon/internal/agent/identity"
)

// Trust fixtures must not live beneath the world-writable system temporary
// directory: safe ancestors are part of the trust boundary, including in tests.
func secureCADirectory(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX administrator ownership")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".orbitbeacon-ca-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("CA validation unexpectedly sent a network request")
	}))
	t.Cleanup(server.Close)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}

func TestCABundleOpensExpectedOwnerAndReadsOnlyVerifiedDescriptor(t *testing.T) {
	dir := secureCADirectory(t)
	path := filepath.Join(dir, "ca.pem")
	trusted := testCAPEM(t)
	if err := os.WriteFile(path, trusted, 0644); err != nil {
		t.Fatal(err)
	}
	opened, err := openCABundle(path, os.Geteuid())
	if err != nil {
		t.Fatalf("secure expected-owner CA rejected: %v", err)
	}
	defer opened.Close()
	before, err := opened.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement must never become trust input"), 0666); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(opened)
	if err != nil || !bytes.Equal(data, trusted) {
		t.Fatalf("trust read was not tied to the validated inode: %q %v", data, err)
	}
	after, err := opened.Stat()
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("CA descriptor changed inode: %v", err)
	}
	if file, err := openCABundle(path+".old", os.Geteuid()+1); err == nil {
		_ = file.Close()
		t.Fatal("unexpected-owner CA was trusted")
	}
}

func TestCABundleRejectsUnsafeFileAndAncestorBeforeTrustLoading(t *testing.T) {
	for _, kind := range []string{"relative path", "final symlink", "ancestor symlink", "group writable file", "other writable file", "group writable ancestor", "other writable ancestor", "directory", "oversized file", "unexpected owner"} {
		t.Run(kind, func(t *testing.T) {
			dir := secureCADirectory(t)
			parent := filepath.Join(dir, "trust")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "ca.pem")
			if err := os.WriteFile(path, testCAPEM(t), 0644); err != nil {
				t.Fatal(err)
			}
			owner := os.Geteuid()
			switch kind {
			case "relative path":
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				path, err = filepath.Rel(cwd, path)
				if err != nil {
					t.Fatal(err)
				}
			case "final symlink":
				link := path + ".link"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "ancestor symlink":
				link := filepath.Join(dir, "link")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "ca.pem")
			case "group writable file", "other writable file":
				mode := os.FileMode(0664)
				if kind == "other writable file" {
					mode = 0646
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			case "group writable ancestor", "other writable ancestor":
				mode := os.FileMode(0720)
				if kind == "other writable ancestor" {
					mode = 0702
				}
				if err := os.Chmod(parent, mode); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path = parent
			case "oversized file":
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate((1 << 20) + 1)
				_ = file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "unexpected owner":
				owner++
			}
			file, err := openCABundle(path, owner)
			if err == nil {
				_ = file.Close()
				t.Fatalf("unsafe CA accepted: %s", kind)
			}
		})
	}
}

func TestProductionClientDoesNotUseRuntimeOwnedOrWritableTrust(t *testing.T) {
	for _, kind := range []string{"group writable", "runtime owned", "symlink", "relative path"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "runtime owned" && os.Geteuid() == 0 {
				t.Skip("expected-owner rejection is exercised above without root")
			}
			path := filepath.Join(secureCADirectory(t), "ca.pem")
			if err := os.WriteFile(path, testCAPEM(t), 0644); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "group writable":
				if err := os.Chmod(path, 0664); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(path, path+".link"); err != nil {
					t.Fatal(err)
				}
				path += ".link"
			case "relative path":
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				path, err = filepath.Rel(cwd, path)
				if err != nil {
					t.Fatal(err)
				}
			}
			state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(ClientOptions{ServerURL: "https://beacon.example.test", CAFile: path, Identity: state})
			if err == nil {
				client.Close()
				t.Fatal("production client accepted mutable/non-administrator trust before making a request")
			}
		})
	}
}
