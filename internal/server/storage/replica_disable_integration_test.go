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

func TestMySQLDisableWrongConfirmationDoesNotRegisterStandalone(t *testing.T) {
	f := storagetest.Start(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	primary := f.Endpoint("primary")
	single := config.DatabaseConfig{Primary: primary}
	dbs, err := storage.OpenDatabases(ctx, single, dir)
	if err != nil {
		t.Fatal(err)
	}
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('preserved','original')"); err != nil {
			t.Fatal(err)
		}
	})
	if err = dbs.Close(); err != nil {
		t.Fatal(err)
	}
	standby := f.Endpoint("standby")
	ha := config.DatabaseConfig{Primary: primary, Standby: &standby}
	if err = storage.DisableReplication(ctx, ha, dir, "wrong-cluster"); !errors.Is(err, storage.ErrIdentityMismatch) {
		t.Fatalf("wrong confirmation: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "storage-control.db")); !os.IsNotExist(err) {
		t.Errorf("wrong confirmation created control: %v", err)
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
			t.Errorf("wrong confirmation registered %s tables=%d triggers=%d", domain, tables, triggers)
		}
	}
	var value string
	if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT value FROM settings WHERE `key`='preserved'").Scan(&value); err != nil || value != "original" {
		t.Errorf("standalone data changed: %q %v", value, err)
	}
	dbs, err = storage.OpenDatabases(ctx, single, dir)
	if err != nil {
		t.Fatalf("wrong confirmation locked standalone: %v", err)
	}
	defer dbs.Close()
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('still-writable','yes')"); err != nil {
			t.Fatal(err)
		}
	})
}
