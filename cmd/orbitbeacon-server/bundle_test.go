//go:build linux

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/bundle"
)

func TestVerifyBundleCommandAcceptsSignedBundle(t *testing.T) {
	dir := t.TempDir()
	artifact := []byte("server artifact")
	filename := "orbitbeacon-server-linux-amd64"
	if err := os.WriteFile(filepath.Join(dir, filename), artifact, 0600); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	manifest, err := json.Marshal(bundle.Manifest{Format: 1, Version: "1.0.0", ReleaseSeq: 1, ProtocolMin: 1, ProtocolMax: 1, Artifacts: []bundle.Artifact{{OS: "linux", Arch: "amd64", Kind: "server", Filename: filename, Size: int64(len(artifact)), SHA256: hex.EncodeToString(digest[:])}}})
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
	if err := run([]string{"verify-bundle", "--directory", dir, "--public-key", keyPath}); err != nil {
		t.Fatal(err)
	}
}
