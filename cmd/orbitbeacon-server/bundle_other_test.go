//go:build !linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyBundleCommandFailsClosedOnUnsupportedPlatform(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "public.key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify-bundle", "--directory", dir, "--public-key", key}); err == nil {
		t.Fatal("unsupported platform accepted verify-bundle")
	}
}
