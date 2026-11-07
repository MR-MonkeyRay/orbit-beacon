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

func TestHAResetTOTPDoesNotRequireHealthyHistory(t *testing.T) {
	fixture := storagetest.Start(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }, Flags: map[string]string{"data_dir": dir}})
	if err != nil {
		t.Fatal(err)
	}
	standby := fixture.Endpoint("standby")
	cfg.Database.Primary, cfg.Database.Standby = fixture.Endpoint("primary"), &standby
	databases, err := storage.OpenDatabases(context.Background(), cfg.Database, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { databases.Close() })
	account, err := auth.NewService(databases.Main, auth.Options{}).CreateUser(context.Background(), "ha-rescue", "ha-rescue-password", false)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := databases.Main.Acquire(context.Background(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(lease.Context(), "UPDATE users SET totp_ciphertext=? WHERE id=?", []byte{1}, account.ID); err != nil {
		tx.Rollback()
		lease.Close()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		lease.Close()
		t.Fatal(err)
	}
	lease.Close()
	if err = databases.Close(); err != nil {
		t.Fatal(err)
	}
	// 只破坏本次隔离夹具的历史表，证明救援不会打开无关历史域。
	if _, err = fixture.RawEndpoint(t, "primary", storage.HistoryDomain).Exec("DROP TABLE buckets"); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "server.json")
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfgPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"reset-totp", "--config", cfgPath, "--username", account.Username}); err != nil {
		t.Fatalf("unrelated HA history prevented rescue: %v", err)
	}
	raw := fixture.RawEndpoint(t, "primary", storage.MainDomain)
	var factors, audit int
	if err = raw.QueryRow("SELECT count(*) FROM users WHERE id=? AND totp_ciphertext IS NOT NULL", account.ID).Scan(&factors); err != nil {
		t.Fatal(err)
	}
	if err = raw.QueryRow("SELECT count(*) FROM audit_events WHERE action='user.totp.reset' AND target_id=?", account.ID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if factors != 0 || audit != 1 {
		t.Fatalf("factor=%d audit=%d", factors, audit)
	}
	var historyTable int
	if err = fixture.RawEndpoint(t, "primary", storage.HistoryDomain).QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='buckets'").Scan(&historyTable); err != nil {
		t.Fatal(err)
	}
	if historyTable != 0 {
		t.Fatal("account rescue recreated unrelated history")
	}
}
