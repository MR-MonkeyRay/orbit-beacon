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
	"strings"
	"testing"
)

func TestVerifyBundleAuthenticatesRawManifestAndArtifacts(t *testing.T) {
	dir := t.TempDir()
	artifact := []byte("release artifact")
	filename := "orbitbeacon-agent-linux-amd64"
	if err := os.WriteFile(filepath.Join(dir, filename), artifact, 0600); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Format: 1, Version: "1.2.3", ReleaseSeq: 7, ProtocolMin: 1, ProtocolMax: 1, Artifacts: []Artifact{{OS: "linux", Arch: "amd64", Kind: "agent", Filename: filename, Size: int64(len(artifact)), SHA256: ""}}}
	manifest.Artifacts[0].SHA256 = sha256Hex(artifact)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.sig"), ed25519.Sign(privateKey, manifestBytes), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "public.key")
	if err := os.WriteFile(keyPath, publicKey, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := VerifyBundle(dir, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != manifest.Version || got.Artifacts[0].Filename != filename {
		t.Fatalf("verified manifest=%+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(dir, keyPath); err == nil {
		t.Fatal("tampered artifact was accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, filename)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, filename)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(dir, keyPath); err == nil {
		t.Fatal("symlink artifact was accepted")
	}
}

func TestVerifyBundleRejectsUnsafeArtifactNamesAndBadSignatures(t *testing.T) {
	for name := range map[string]bool{"../escape": true, "nested/file": true, "": true} {
		manifest := Manifest{Format: 1, Version: "1", ReleaseSeq: 1, ProtocolMin: 1, ProtocolMax: 1, Artifacts: []Artifact{{OS: "linux", Arch: "amd64", Kind: "agent", Filename: name, Size: 0, SHA256: sha256Hex(nil)}}}
		if err := manifest.Validate(); err == nil {
			t.Fatalf("unsafe filename %q accepted", name)
		}
	}
	uppercase := Manifest{Format: 1, Version: "1", ReleaseSeq: 1, ProtocolMin: 1, ProtocolMax: 1, Artifacts: []Artifact{{OS: "linux", Arch: "amd64", Kind: "agent", Filename: "artifact", Size: 0, SHA256: strings.Repeat("A", 64)}}}
	if err := uppercase.Validate(); err == nil {
		t.Fatal("uppercase SHA-256 was accepted")
	}
}

func TestVerifyBundleRejectsInvalidUTF8Manifest(t *testing.T) {
	dir := t.TempDir()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := append([]byte(`{"format":1,"version":"`), 0xff)
	manifest = append(manifest, []byte(`","release_seq":1}`)...)
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
	if _, err := VerifyBundle(dir, keyPath); err == nil {
		t.Fatal("invalid UTF-8 manifest was accepted")
	}
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
