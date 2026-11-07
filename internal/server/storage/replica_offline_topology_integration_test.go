package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLFirstOfflineRestoreRejectsAliasedPhysicalStandby(t *testing.T) {
	f := storagetest.Start(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	primary := f.Endpoint("primary")
	single := config.DatabaseConfig{Primary: primary}
	original, err := storage.OpenDatabases(ctx, single, dir)
	if err != nil {
		t.Fatal(err)
	}
	haWrite(t, original.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('alias-preserved','original')"); err != nil {
			t.Fatal(err)
		}
	})
	original.Close()
	_, alias := newReplicaProxy(t, primary)
	if alias.Address == primary.Address {
		t.Fatal("fixture did not create a distinct endpoint")
	}
	ha := config.DatabaseConfig{Primary: primary, Standby: &alias}
	// 离线 Restore 必须先通过此入口，错误物理拓扑不能取得恢复写入资格。
	opened, err := storage.OpenOfflineDatabases(ctx, ha, dir)
	if opened != nil {
		opened.Close()
	}
	if !errors.Is(err, storage.ErrIdentityMismatch) {
		t.Errorf("offline restore accepted same physical databases: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "storage-control.db")); !os.IsNotExist(err) {
		t.Errorf("invalid topology created control: %v", err)
	}
	for _, domain := range []storage.Domain{storage.MainDomain, storage.HistoryDomain} {
		raw := f.RawEndpoint(t, "primary", domain)
		var tables, triggers int
		if err = raw.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('replica_identity','replica_state','replica_epochs','replica_transactions','replica_changes')").Scan(&tables); err != nil {
			t.Fatal(err)
		}
		if err = raw.QueryRow("SELECT count(*) FROM information_schema.triggers WHERE trigger_schema=DATABASE()").Scan(&triggers); err != nil {
			t.Fatal(err)
		}
		if tables != 0 || triggers != 0 {
			t.Errorf("invalid offline topology registered %s tables=%d triggers=%d", domain, tables, triggers)
		}
	}
	var value string
	if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT value FROM settings WHERE `key`='alias-preserved'").Scan(&value); err != nil || value != "original" {
		t.Errorf("source data changed: %q %v", value, err)
	}
	reopened, err := storage.OpenDatabases(ctx, single, dir)
	if err != nil {
		t.Fatalf("invalid offline topology locked standalone: %v", err)
	}
	defer reopened.Close()
	haWrite(t, reopened.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('after-alias-rejection','writable')"); err != nil {
			t.Fatal(err)
		}
	})
}
