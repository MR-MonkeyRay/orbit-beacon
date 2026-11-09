package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestRestoreAndResetTOTPAcceptConfiguredDataDirectory(t *testing.T) {
	archive, passwordFile := commandArchiveFixture(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "restored")
	cfgPath := filepath.Join(t.TempDir(), "server.json")
	body, err := json.Marshal(map[string]any{"data_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfgPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"restore", "--config", cfgPath, "--input", archive, "--password-file", passwordFile}); err != nil {
		t.Fatalf("configured restore: %v", err)
	}
	if err = run([]string{"reset-totp", "--config", cfgPath, "--username", "archive-admin"}); err != nil {
		t.Fatalf("configured reset: %v", err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='user.totp.reset'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reset audit=%d/%v", count, err)
	}
}
