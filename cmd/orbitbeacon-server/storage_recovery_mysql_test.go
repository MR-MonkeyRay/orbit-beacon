package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLStorageCLIRebuildRetainsSourceAndDoesNotRepeatTerminalOperation(t *testing.T) {
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
	ctx := context.Background()
	databases, err := storage.OpenDatabases(ctx, cfg.Database, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer databases.Close()
	lease, err := databases.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(lease.Context(), "INSERT INTO settings VALUES('cli-keep','source-value')"); err != nil {
		tx.Rollback()
		lease.Close()
		t.Fatal(err)
	}
	err = tx.Commit()
	lease.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = databases.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		status, statusErr := databases.RecoveryStatus(ctx)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.Domains[0].Standby != nil && status.Domains[0].Standby.Ready && status.Domains[1].Standby != nil && status.Domains[1].Standby.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI fixture did not finish both registered copies")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = databases.Close(); err != nil {
		t.Fatal(err)
	}
	primaryRaw, targetRaw := fixture.RawEndpoint(t, "primary", storage.MainDomain), fixture.RawEndpoint(t, "standby", storage.MainDomain)
	if _, err = targetRaw.Exec("INSERT INTO settings VALUES('cli-target-only','discard')"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "server.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = storageCommandOutput(t, "storage", "preview", "--config", configPath, "--domain", "main", "--action", "rebuild", "--target", "primary"); !errors.Is(err, storage.ErrManualRequired) {
		t.Fatalf("active target accepted: %v", err)
	}
	raw, err = storageCommandOutput(t, "storage", "preview", "--config", configPath, "--domain", "main", "--action", "rebuild", "--target", "standby", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var preview storage.RecoveryPreview
	if err = json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	args := []string{"storage", "run", "--config", configPath, "--operation", preview.OperationID, "--confirm", preview.Confirmation, "--json"}
	raw, err = storageCommandOutput(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	var operation storage.RecoveryOperation
	if err = json.Unmarshal(raw, &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Status != "succeeded" || operation.ID != preview.OperationID {
		t.Fatalf("CLI did not finish requested rebuild: %+v", operation)
	}
	for role, raw := range map[string]*sql.DB{"primary": primaryRaw, "standby": targetRaw} {
		var value string
		if err = raw.QueryRow("SELECT value FROM settings WHERE `key`='cli-keep'").Scan(&value); err != nil || value != "source-value" {
			t.Fatalf("%s lost preserved source: %q %v", role, value, err)
		}
	}
	var count int
	if err = targetRaw.QueryRow("SELECT count(*) FROM settings WHERE `key`='cli-target-only'").Scan(&count); err != nil || count != 0 {
		t.Fatal("inactive target was not rebuilt", count, err)
	}
	if _, err = targetRaw.Exec("INSERT INTO settings VALUES('cli-after-terminal','retain')"); err != nil {
		t.Fatal(err)
	}
	if _, err = storageCommandOutput(t, args...); err != nil {
		t.Fatal(err)
	}
	if err = targetRaw.QueryRow("SELECT count(*) FROM settings WHERE `key`='cli-after-terminal'").Scan(&count); err != nil || count != 1 {
		t.Fatal("terminal resubmit rebuilt target again", count, err)
	}
	reopened, err := storage.OpenRecoveryDatabases(ctx, cfg.Database, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	operation, err = reopened.RecoveryOperation(ctx, "local", preview.OperationID)
	if err != nil || operation.Status != "succeeded" {
		t.Fatal("terminal operation not queryable", operation, err)
	}
}
