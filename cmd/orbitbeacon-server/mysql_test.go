package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLResetTOTPUsesConfiguredRemoteStore(t *testing.T) {
	fixture := storagetest.Start(t)
	endpoint := fixture.Endpoint("primary")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	databases, err := storage.OpenDatabases(context.Background(), config.DatabaseConfig{Primary: endpoint}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer databases.Close()
	account, err := auth.NewService(databases.Main, auth.Options{}).CreateUser(context.Background(), "mysql-rescue", "mysql-rescue-password", false)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := databases.Main.Acquire(context.Background(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(lease.Context(), `UPDATE users SET totp_ciphertext=? WHERE id=?`, []byte{1}, account.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err = databases.Close(); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "server.json")
	encoded, err := json.Marshal(map[string]any{"data_dir": dir, "database": config.DatabaseConfig{Primary: endpoint}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfgPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"reset-totp", "--config", cfgPath, "--username", account.Username}); err != nil {
		t.Fatal(err)
	}
	databases, err = storage.OpenDatabases(context.Background(), config.DatabaseConfig{Primary: endpoint}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer databases.Close()
	reader, err := databases.Main.Acquire(context.Background(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var enabled int
	if err = reader.QueryRowContext(reader.Context(), `SELECT count(*) FROM users WHERE id=? AND totp_ciphertext IS NOT NULL`, account.ID).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("remote factor retained: %d/%v", enabled, err)
	}
	var auditCount int
	if err = reader.QueryRowContext(reader.Context(), `SELECT count(*) FROM audit_events WHERE action='user.totp.reset' AND target_id=?`, account.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("missing remote reset audit: %d/%v", auditCount, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "orbitbeacon.db")); !os.IsNotExist(err) {
		t.Fatalf("CLI created unintended local store: %v", err)
	}
}
