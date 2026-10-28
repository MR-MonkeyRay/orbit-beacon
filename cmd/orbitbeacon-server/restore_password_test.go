package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestorePasswordFileRequiresPrivateExactGeneratedPassword(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "password.txt")
	for _, suffix := range []string{"", "\n"} {
		if err := os.WriteFile(path, []byte("aB0123456789cDef"+suffix), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := readRestorePassword(path)
		if err != nil || got != "aB0123456789cDef" {
			t.Fatalf("password length=%d err=%v", len(got), err)
		}
	}
	for _, raw := range []string{"", "aB0123456789cDef\n\n", "aB0123456789cDef\r\n", "too short", strings.Repeat("a", 17), "aB0123456789cDe!"} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readRestorePassword(path); err == nil {
			t.Fatal("invalid recovery password accepted")
		}
	}
	if err := os.WriteFile(path, []byte("aB0123456789cDef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRestorePassword(path); err == nil {
		t.Fatal("public password file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRestorePassword(link); err == nil {
		t.Fatal("password symlink accepted")
	}
}
