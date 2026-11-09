package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

// committed 是安装决定，不是要求再次写入各域；重复发布不能覆盖其后的正常提交。
func TestRemoteRestoreCommittedRecoveryDoesNotReinstall(t *testing.T) {
	dir := restoreFixture(t, "target")
	next := restoreFixture(t, "source")
	cfg := restoreConfig(dir)
	configPath := filepath.Join(t.TempDir(), "server.json")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	runRemoteCrash(t, configPath, next, "finish:storage:durable", "commit")
	domains, err := storage.OpenOfflineDatabases(t.Context(), cfg.Database, dir)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := domains.Main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(lease.Context(), `UPDATE nodes SET name='after-publication' WHERE id=?`, restoreNode); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	domains.Close()
	if err = RecoverRestore(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	db := restoreDB(t, dir)
	defer db.Close()
	var name string
	if err = db.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "after-publication" {
		t.Fatalf("committed recovery reinstalled data: name=%q err=%v", name, err)
	}
}

func TestRemoteRestoreMetadataVerifiedBeforeAnyDomainRollback(t *testing.T) {
	for _, mutation := range []string{"missing", "changed", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			dir := restoreFixture(t, "target")
			next := restoreFixture(t, "source")
			cfg := restoreConfig(dir)
			configPath := filepath.Join(t.TempDir(), "server.json")
			body, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(configPath, body, 0600); err != nil {
				t.Fatal(err)
			}
			runRemoteCrash(t, configPath, next, "install:main:committed", "commit")
			journal, err := readRestoreJournal(filepath.Join(dir, restoreJournalName))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".restore-"+journal.ID, "old", restoreMetadataName)
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "changed":
				err = os.WriteFile(path, []byte(`{"changed":true}`), 0600)
			case "symlink":
				err = os.Symlink(filepath.Join(next, "secrets.key"), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = RecoverRestore(t.Context(), cfg); restoreCode(err) != "restore_recovery_failed" {
				t.Fatalf("invalid metadata accepted: %v", err)
			}
			db := restoreDB(t, dir)
			defer db.Close()
			var name string
			if err = db.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "source" {
				t.Fatalf("partial rollback name=%q err=%v", name, err)
			}
			if _, err = os.Stat(filepath.Join(dir, restoreJournalName)); err != nil {
				t.Fatalf("pending decision lost: %v", err)
			}
		})
	}
}

func TestRemoteRestoreMetadataIsDurableBeforePrepared(t *testing.T) {
	dir := restoreFixture(t, "target")
	next := restoreFixture(t, "source")
	cfg := restoreConfig(dir)
	domains, err := storage.OpenOfflineDatabases(t.Context(), cfg.Database, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer domains.Close()
	key, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	ops := osFSOps()
	syncFile := ops.fileSync
	ops.fileSync = func(label string, file *os.File) error {
		if label == "old:storage-metadata:file-sync" {
			return errors.New("injected metadata persistence failure")
		}
		return syncFile(label, file)
	}
	if _, err = commitRemoteRestore(t.Context(), cfg, domains, next, ops); err == nil {
		t.Fatal("unsynced metadata accepted")
	}
	if _, err = os.Stat(filepath.Join(dir, restoreJournalName)); !os.IsNotExist(err) {
		t.Fatalf("prepared before metadata fsync: %v", err)
	}
	got, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("old key changed: %v", err)
	}
	clear(got)
	lease, err := domains.Main.Acquire(context.Background(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var name string
	if err = lease.QueryRowContext(lease.Context(), `SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "target" {
		t.Fatalf("target changed before prepared: name=%q err=%v", name, err)
	}
}

func TestRestoreDoesNotBypassRegisteredControlWithSQLiteConfig(t *testing.T) {
	dir := restoreFixture(t, "target")
	next := restoreFixture(t, "source")
	archive, password := restoreArchive(t, next, []Category{Configuration, Monitoring, Events})
	if err := os.WriteFile(filepath.Join(dir, "storage-control.db"), []byte("invalid control"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(t.Context(), restoreConfig(dir), archive, password, true); err == nil {
		t.Fatal("SQLite file restore bypassed existing HA control")
	}
	db := restoreDB(t, dir)
	defer db.Close()
	var name string
	if err := db.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "target" {
		t.Fatalf("existing control bypass changed target: name=%q err=%v", name, err)
	}
}
