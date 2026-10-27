//go:build linux

package bundle

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestVerifyBundleRejectsFIFOsWithoutBlocking(t *testing.T) {
	for _, name := range []string{"manifest", "artifact", "public key"} {
		t.Run(name, func(t *testing.T) {
			dir, keyPath, artifactPath := writeFIFOTestBundle(t)
			var target string
			switch name {
			case "manifest":
				target = filepath.Join(dir, "manifest.json")
			case "artifact":
				target = artifactPath
			case "public key":
				target = keyPath
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(target, 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := VerifyBundle(dir, keyPath)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("FIFO was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("FIFO verification blocked")
			}
		})
	}
}

func writeFIFOTestBundle(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	artifact := []byte("artifact")
	artifactPath := filepath.Join(dir, "artifact")
	if err := os.WriteFile(artifactPath, artifact, 0600); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	manifest, err := json.Marshal(Manifest{Format: 1, Version: "1", ReleaseSeq: 1, ProtocolMin: 1, ProtocolMax: 1, Artifacts: []Artifact{{OS: "linux", Arch: "amd64", Kind: "test", Filename: "artifact", Size: int64(len(artifact)), SHA256: hex.EncodeToString(digest[:])}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.sig"), ed25519.Sign(privateKey, manifest), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "public.key")
	if err := os.WriteFile(keyPath, publicKey, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, keyPath, artifactPath
}
