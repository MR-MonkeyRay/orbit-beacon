//go:build !linux

package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyBundleFailsClosedOnUnsupportedPlatform(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "public.key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(dir, key); err == nil {
		t.Fatal("unsupported platform accepted bundle verification")
	}
}
