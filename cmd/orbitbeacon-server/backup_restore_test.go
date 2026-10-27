package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestBackupAndRestoreUseConsistentSQLiteSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(dataDir, "orbitbeacon.db")
	db, err := storage.Open(context.Background(), livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES ('drill','before')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	if err := os.Chmod(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(backupDir, "orbitbeacon.db.backup")
	if err := backupDatabase(dataDir, backupPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions=%o, want 0600", info.Mode().Perm())
	}
	db, err = storage.Open(context.Background(), livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE settings SET value='mutated' WHERE key='drill'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.WriteFile(livePath+suffix, []byte("stale sidecar"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := restoreDatabase(dataDir, backupPath); err != nil {
		t.Fatal(err)
	}
	db, err = storage.Open(context.Background(), livePath)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key='drill'`).Scan(&value); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if value != "before" {
		_ = db.Close()
		t.Fatalf("restored value=%q, want before", value)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(livePath + suffix); !os.IsNotExist(err) {
			t.Fatalf("restored database retained live sidecar %s: %v", suffix, err)
		}
	}
	previous, err := filepath.Glob(filepath.Join(dataDir, ".orbitbeacon.db.before-restore-*"))
	if err != nil || len(previous) != 1 {
		t.Fatalf("pre-restore database was not retained: matches=%v err=%v", previous, err)
	}
	configPath := filepath.Join(t.TempDir(), "server.json")
	configData, err := json.Marshal(map[string]any{"data_dir": dataDir, "public_url": "https://backup.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}
	cliBackup := filepath.Join(backupDir, "cli.backup")
	if err := run([]string{"backup", "--config", configPath, "--output", cliBackup}); err != nil {
		t.Fatalf("backup CLI wiring: %v", err)
	}
	if err := run([]string{"restore", "--data-dir", dataDir, "--input", cliBackup}); err != nil {
		t.Fatalf("restore CLI wiring: %v", err)
	}
}

func TestConcurrentBackupsDoNotDeleteWinner(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dataDir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	if err := os.Chmod(outputDir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, "same.backup")
	errs := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- backupDatabase(dataDir, output)
		}()
	}
	group.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent backups succeeded %d times, want exactly once", successes)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("winning backup was removed: %v", err)
	}
}

func TestRestoreFailureCleansStagingArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	inputDir := t.TempDir()
	if err := os.Chmod(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(inputDir, "invalid.backup")
	if err := os.WriteFile(input, []byte("not a sqlite database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, input); err == nil {
		t.Fatal("restore accepted an invalid database")
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, ".orbitbeacon-restore-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("restore staging artifacts remain: %v", matches)
	}
}

func TestBackupRejectsMissingSourceDatabase(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	if err := os.Chmod(outputDir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, "missing.backup")
	if err := backupDatabase(dataDir, output); err == nil {
		t.Fatal("backup accepted a missing source database")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("missing-source backup created output: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "server.lock")); !os.IsNotExist(err) {
		t.Fatalf("missing-source backup mutated data directory: %v", err)
	}
}

func TestRestoreRejectsSchemaLedgerWithoutTables(t *testing.T) {
	inputDir := t.TempDir()
	if err := os.Chmod(inputDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(inputDir, "invalid-schema.db")
	db, err := storage.Open(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, table := range requiredOrbitBeaconTables[:len(requiredOrbitBeaconTables)-1] {
		if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(dataDir, input); err == nil {
		t.Fatal("restore accepted a schema ledger without application tables")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "orbitbeacon.db")); !os.IsNotExist(err) {
		t.Fatalf("invalid schema installed a live database: %v", err)
	}
}

func TestRestoreRejectsMalformedIdentityIndex(t *testing.T) {
	tests := []struct {
		name  string
		index string
	}{
		{name: "wrong table", index: "CREATE INDEX nodes_installation_identity ON groups(name)"},
		{name: "not unique or partial", index: "CREATE INDEX nodes_installation_identity ON nodes(installation_id)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputDir := t.TempDir()
			if err := os.Chmod(inputDir, 0700); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(inputDir, "invalid-index.db")
			db, err := storage.Open(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DROP INDEX nodes_installation_identity`); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if _, err := db.Exec(test.index); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			dataDir := t.TempDir()
			if err := os.Chmod(dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := restoreDatabase(dataDir, input); err == nil {
				t.Fatal("restore accepted a malformed identity index")
			}
			if _, err := os.Lstat(filepath.Join(dataDir, "orbitbeacon.db")); !os.IsNotExist(err) {
				t.Fatalf("malformed index installed a live database: %v", err)
			}
		})
	}
}
