package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResetTOTPDoesNotRequireReadableHistory(t *testing.T) {
	dir, username := enrolledDataDir(t)
	history := filepath.Join(dir, "metrics.db")
	original := []byte("unreadable retained history")
	if err := os.WriteFile(history, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"reset-totp", "--data-dir", dir, "--username", username}); err != nil {
		t.Fatalf("unrelated history blocked account rescue: %v", err)
	}
	if count := secondFactorCount(t, dir); count != 0 {
		t.Fatalf("second factor rows=%d", count)
	}
	got, err := os.ReadFile(history)
	if err != nil || string(got) != string(original) {
		t.Fatalf("reset changed history: %q/%v", got, err)
	}
}
