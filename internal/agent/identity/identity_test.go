package identity_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"orbitbeacon/internal/agent/identity"
)

func TestIdentityCreatedOnceBeforeUseAndRunGenerationPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "identity.json")
	store, err := identity.Open(path, identity.Options{Random: rand.NewChaCha8([32]byte{41})})
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Current()
	if initial.InstallationID == "" || initial.RunGeneration != 0 {
		t.Fatalf("initial identity=%+v", initial)
	}
	token, err := base64.RawURLEncoding.DecodeString(initial.NodeToken)
	if err != nil || len(token) != 32 {
		t.Fatalf("node token is not 256 bits: %v", err)
	}
	var saved identity.Identity
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("identity not persisted before Open returns: %v", err)
	}
	if err := json.Unmarshal(data, &saved); err != nil || !reflect.DeepEqual(saved, initial) {
		t.Fatalf("saved=%+v initial=%+v err=%v", saved, initial, err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	first, err := store.BeginRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.RunGeneration != 1 || first.SessionID == "" || first.InstallationID != initial.InstallationID || first.NodeToken != initial.NodeToken {
		t.Fatalf("first run=%+v", first)
	}
	// Reopening for enrollment, doctor or watch is read-only with respect to generation.
	reopened, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if reopened.Current().RunGeneration != 1 {
			t.Fatal("inspection incremented generation")
		}
	}
	second, err := reopened.BeginRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.RunGeneration != 2 || second.SessionID == first.SessionID || second.InstallationID != first.InstallationID || second.NodeToken != first.NodeToken {
		t.Fatalf("second run=%+v", second)
	}
	stillOld, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stillOld, data) {
		t.Fatal("identity file modified in place instead of atomically replaced")
	}
	reloaded, err := identity.Open(path, identity.Options{})
	if err != nil || reloaded.Current().RunGeneration != 2 {
		t.Fatalf("generation not durable: %v", err)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
			info, err := os.Stat(p)
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("permissions %s: %v %v", p, info, err)
			}
		}
	}
}

func TestIdentityNeverRegeneratesCorruptExistingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	broken := []byte(`{"installation_id":"existing","node_token":`)
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Open(path, identity.Options{}); err == nil {
		t.Fatal("corrupt identity silently replaced")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, broken) {
		t.Fatal("corrupt identity was overwritten")
	}
}

func TestIdentityFailureDoesNotPublishPartialIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if _, err := identity.Open(path, identity.Options{Random: bytes.NewReader([]byte{1, 2})}); err == nil {
		t.Fatal("short entropy accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial identity visible: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed creation left identity fragments: %v", entries)
	}
}

func TestIdentityRejectsSymlinksAndUnsafePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions and symlink contract")
	}
	for _, kind := range []string{"symlink", "world-readable"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			real := filepath.Join(dir, "real.json")
			if _, err := identity.Open(real, identity.Options{}); err != nil {
				t.Fatal(err)
			}
			path := real
			if kind == "symlink" {
				path = filepath.Join(dir, "identity.json")
				if err := os.Symlink(real, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(real, 0644); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(real)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := identity.Open(path, identity.Options{}); err == nil {
				t.Fatal("unsafe identity accepted")
			}
			after, err := os.ReadFile(real)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unsafe identity modified")
			}
		})
	}
}
