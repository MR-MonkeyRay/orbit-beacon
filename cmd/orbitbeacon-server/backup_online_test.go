package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func onlineBackupFixture(t *testing.T) (string, string, *sql.DB) {
	t.Helper()
	dataDir, outputDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{dataDir, outputDir} {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := storage.Open(context.Background(), filepath.Join(dataDir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return dataDir, filepath.Join(outputDir, "snapshot.db"), db
}

func TestOnlineBackupWhileLockedAndWritingWALRestores(t *testing.T) {
	dataDir, output, db := onlineBackupFixture(t)
	lock, err := datadir.Lock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := backupDatabase(dataDir, output); !errors.Is(err, datadir.ErrInUse) {
		t.Fatalf("offline backup with held lock: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0;
		INSERT INTO settings(key,value) VALUES ('wal-marker','committed'), ('counter','0');
		WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<512)
		INSERT INTO settings(key,value) SELECT 'payload-'||x, hex(randomblob(2048)) FROM n`); err != nil {
		t.Fatal(err)
	}
	wal, err := os.Stat(filepath.Join(dataDir, "orbitbeacon.db-wal"))
	if err != nil || wal.Size() == 0 {
		t.Fatalf("expected outstanding WAL data: %v", err)
	}
	var writes atomic.Int64
	stop, started := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if _, err := db.Exec(`UPDATE settings SET value=? WHERE key='counter'`, strconv.FormatInt(i, 10)); err != nil {
				if i == 1 {
					close(started)
				}
				done <- err
				return
			}
			writes.Store(i)
			if i == 1 {
				close(started)
			}
		}
	}()
	<-started
	before := writes.Load()
	err = run([]string{"backup", "--online", "--data-dir", dataDir, "--output", output})
	after := writes.Load()
	close(stop)
	if writeErr := <-done; writeErr != nil {
		t.Fatalf("WAL writer: %v", writeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("writer made no progress during backup: before=%d after=%d", before, after)
	}
	if _, err := db.Exec(`UPDATE settings SET value='after-backup' WHERE key='wal-marker'`); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode=%o, want 0600", info.Mode().Perm())
	}
	restoredDir := t.TempDir()
	if err := os.Chmod(restoredDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(restoredDir, output); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.OpenReader(context.Background(), filepath.Join(restoredDir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var marker string
	var counter int64
	if err := restored.QueryRow(`SELECT value FROM settings WHERE key='wal-marker'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if err := restored.QueryRow(`SELECT value FROM settings WHERE key='counter'`).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if marker != "committed" || counter < 1 || counter > writes.Load() {
		t.Fatalf("unexpected snapshot marker=%q counter=%d writes=%d", marker, counter, writes.Load())
	}
	if err := validateDatabase(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "secrets.key")); !os.IsNotExist(err) {
		t.Fatalf("online backup created secrets.key: %v", err)
	}
}

func TestOnlineBackupConfigAndUncommittedWriter(t *testing.T) {
	dataDir, output, db := onlineBackupFixture(t)
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES ('transaction','committed')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE settings SET value='uncommitted' WHERE key='transaction'`); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "server.json")
	configData, err := json.Marshal(map[string]string{"data_dir": dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"backup", "--online", "--config", configPath, "--output", output}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := storage.OpenReader(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var value string
	if err := snapshot.QueryRow(`SELECT value FROM settings WHERE key='transaction'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "committed" {
		t.Fatalf("snapshot contains uncommitted data: %q", value)
	}
}

func TestOnlineBackupRejectsExistingOutput(t *testing.T) {
	dataDir, output, _ := onlineBackupFixture(t)
	const existing = "existing backup"
	if err := os.WriteFile(output, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backupDatabaseOnline(dataDir, output); err == nil {
		t.Fatal("online backup overwrote existing output")
	}
	contents, err := os.ReadFile(output)
	if err != nil || string(contents) != existing {
		t.Fatalf("existing output changed: %q, %v", contents, err)
	}
}

func TestOnlineBackupRejectsInvalidSchemaWithoutWritingSource(t *testing.T) {
	for _, test := range []struct {
		name, mutation string
		tooNew         bool
	}{
		{"newer", fmt.Sprintf(`INSERT INTO schema_migrations VALUES (%d, 'future')`, storage.CurrentSchemaVersion+1), true},
		{"empty ledger", `DELETE FROM schema_migrations`, false},
		{"missing ledger", `DROP TABLE schema_migrations`, false},
		{"invalid tables", `DROP TABLE settings`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataDir, output, db := onlineBackupFixture(t)
			if _, err := db.Exec(test.mutation); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(dataDir, "orbitbeacon.db")
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			err = backupDatabaseOnline(dataDir, output)
			if err == nil || (test.tooNew && !errors.Is(err, storage.ErrSchemaTooNew)) {
				t.Fatalf("invalid schema backup error: %v", err)
			}
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("source database changed: %v", err)
			}
			assertOnlineBackupAbsent(t, output)
		})
	}
}

func TestOnlineBackupNeverCreatesMissingSource(t *testing.T) {
	for _, missingDir := range []bool{false, true} {
		t.Run(fmt.Sprint(missingDir), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			dataDir := filepath.Join(root, "data")
			if !missingDir {
				if err := os.Mkdir(dataDir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(root, "snapshot.db")
			if err := backupDatabaseOnline(dataDir, output); err == nil {
				t.Fatal("online backup accepted missing source")
			}
			for _, name := range []string{"orbitbeacon.db", "server.lock", "secrets.key"} {
				if _, err := os.Lstat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
					t.Fatalf("online backup created %s: %v", name, err)
				}
			}
			assertOnlineBackupAbsent(t, output)
		})
	}
}

func TestOnlineBackupDoesNotMigrateSource(t *testing.T) {
	dataDir, output, db := onlineBackupFixture(t)
	// Replay the original data-only migration in the backup copy; retain the
	// ledger for structural migrations whose tables already exist.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version=9`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dataDir, "orbitbeacon.db")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := backupDatabaseOnline(dataDir, output); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("online backup migrated or modified source: %v", err)
	}
	reader, err := openOnlineBackupSource(source)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	reader.SetMaxOpenConns(1)
	if _, err := reader.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Exec(`INSERT INTO settings VALUES ('should-not-write','forbidden')`); err == nil {
		t.Fatal("mode=ro source connection accepted a write")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "server.lock")); !os.IsNotExist(err) {
		t.Fatalf("online backup created server lock: %v", err)
	}
}

func TestOnlineBackupRejectsUnsafePaths(t *testing.T) {
	for _, target := range []string{"data directory permissions", "data directory symlink", "database permissions", "database symlink", "-wal", "-shm", "-journal", "output symlink", "output ancestor symlink"} {
		t.Run(target, func(t *testing.T) {
			dataDir, output, db := onlineBackupFixture(t)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(dataDir, "orbitbeacon.db")
			switch target {
			case "data directory permissions":
				if err := os.Chmod(dataDir, 0750); err != nil {
					t.Fatal(err)
				}
			case "data directory symlink":
				link := filepath.Join(filepath.Dir(output), "data-link")
				if err := os.Symlink(dataDir, link); err != nil {
					t.Fatal(err)
				}
				dataDir = link
			case "database permissions":
				if err := os.Chmod(source, 0644); err != nil {
					t.Fatal(err)
				}
			case "database symlink":
				if err := os.Rename(source, source+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(source+".real", source); err != nil {
					t.Fatal(err)
				}
			case "output symlink":
				if err := os.Symlink(source, output); err != nil {
					t.Fatal(err)
				}
			case "output ancestor symlink":
				link := filepath.Join(dataDir, "output-link")
				if err := os.Symlink(filepath.Dir(output), link); err != nil {
					t.Fatal(err)
				}
				output = filepath.Join(link, "snapshot.db")
			default:
				if err := os.Symlink(source, source+target); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := backupDatabaseOnline(dataDir, output); err == nil {
				t.Fatal("online backup accepted unsafe path")
			}
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("source database changed: %v", err)
			}
			if target != "output symlink" {
				assertOnlineBackupAbsent(t, output)
			}
		})
	}
}

func assertOnlineBackupAbsent(t *testing.T, output string) {
	t.Helper()
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("failed online backup published output: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".orbitbeacon-backup-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("backup staging artifacts remain: %v, %v", matches, err)
	}
}
