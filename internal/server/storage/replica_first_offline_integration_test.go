package storage_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLRegisteredOfflineMainOnlyResetEnablesCaptureWithoutHistory(t *testing.T) {
	ctx := context.Background()
	f := storagetest.Start(t)
	base := f.Open(t, storage.MainDomain)
	haWrite(t, base, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,created_at,totp_ciphertext) VALUES('offline','first-ha-offline','hash','now',X'01')"); err != nil {
			t.Fatal(err)
		}
	})
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}
	standby := f.Endpoint("standby")
	cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	registered, err := storage.OpenOfflineDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := registered.CaptureRestoreMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var control struct{ Pending string }
	if err = json.Unmarshal(metadata, &control); err != nil || control.Pending != "" {
		t.Fatal("setup already entered restore", err)
	}
	if err = registered.Close(); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT capture_mode FROM replica_state WHERE id=1").Scan(&mode); err != nil || mode != "off" {
		t.Fatal("setup did not leave registered uncaptured source", mode, err)
	}
	primaryHistory, standbyHistory := f.RawEndpoint(t, "primary", storage.HistoryDomain), f.RawEndpoint(t, "standby", storage.HistoryDomain)
	for _, history := range []string{"primary", "standby"} {
		raw := f.RawEndpoint(t, history, storage.HistoryDomain)
		if _, err := raw.ExecContext(ctx, "CREATE TABLE unrelated_history(marker BIGINT NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, "INSERT INTO unrelated_history VALUES(73)"); err != nil {
			t.Fatal(err)
		}
	}
	beforePrimary, beforeStandby := replicaNativeObjects(t, primaryHistory), replicaNativeObjects(t, standbyHistory)
	db, err := storage.OpenActiveDomain(ctx, cfg, storage.MainDomain, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = auth.ResetTOTP(audit.WithSystemActor(ctx, "reset-totp"), db, "first-ha-offline", time.Now()); err != nil {
		t.Error("first registered main-only offline writer rejected", err)
	} else {
		raw := f.RawEndpoint(t, "primary", storage.MainDomain)
		var reset bool
		var audits, changes int
		if err = raw.QueryRow("SELECT totp_ciphertext IS NULL FROM users WHERE id='offline'").Scan(&reset); err != nil || !reset {
			t.Error("real reset did not update account", reset, err)
		}
		if err = raw.QueryRow("SELECT count(*) FROM audit_events WHERE action='user.totp.reset'").Scan(&audits); err != nil || audits != 1 {
			t.Error("reset audit missing", audits, err)
		}
		if err = raw.QueryRow("SELECT change_count FROM replica_transactions WHERE tx_uuid=(SELECT head_tx_uuid FROM replica_state WHERE id=1)").Scan(&changes); err != nil || changes != 2 {
			t.Error("reset and audit were not captured in one source transaction", changes, err)
		}
	}
	if !reflect.DeepEqual(beforePrimary, replicaNativeObjects(t, primaryHistory)) || !reflect.DeepEqual(beforeStandby, replicaNativeObjects(t, standbyHistory)) {
		t.Error("main-only reset initialized or altered unreadable history")
	}
	for _, raw := range []string{"primary", "standby"} {
		var marker int
		if err = f.RawEndpoint(t, raw, storage.HistoryDomain).QueryRow("SELECT marker FROM unrelated_history").Scan(&marker); err != nil || marker != 73 {
			t.Error("main-only reset changed unrelated history", marker, err)
		}
	}
	var tables int
	if err = f.RawEndpoint(t, "standby", storage.MainDomain).QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil || tables != 0 {
		t.Error("main-only reset initialized standby before StartReplication", tables, err)
	}
}
