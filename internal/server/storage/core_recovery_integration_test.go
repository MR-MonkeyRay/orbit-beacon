package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

type coreMember struct {
	Active, State, Reason   string
	Ready, Manual           bool
	Freeze, Intent, Rebuild json.RawMessage
}

func coreMembers(t *testing.T, dbs *storage.Databases) map[storage.Domain]coreMember {
	t.Helper()
	raw, err := dbs.CaptureRestoreMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ Domains map[storage.Domain]coreMember }
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record.Domains
}
func controlValue(raw json.RawMessage) bool { return len(raw) != 0 && string(raw) != "null" }
func waitCore(t *testing.T, dbs *storage.Databases, accept func(map[storage.Domain]coreMember) bool) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if accept(coreMembers(t, dbs)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("control prerequisite did not converge: %+v", coreMembers(t, dbs))
}
func coreConfig(t *testing.T, f *storagetest.Fixture) (config.DatabaseConfig, string) {
	t.Helper()
	standby := f.Endpoint("standby")
	cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: time.Second, ManualAfter: time.Hour}}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}
func completeCoreSeeds(t *testing.T, dbs *storage.Databases) {
	t.Helper()
	waitCore(t, dbs, func(m map[storage.Domain]coreMember) bool {
		return m[storage.MainDomain].Ready && m[storage.HistoryDomain].Ready && !controlValue(m[storage.MainDomain].Rebuild) && !controlValue(m[storage.HistoryDomain].Rebuild)
	})
}

func TestMySQLOfflineMainRejectsDurableFreezeButHealthyHistoryRemainsWritable(t *testing.T) {
	f := storagetest.Start(t)
	cfg, dir := coreConfig(t, f)
	cfg.Failover.FailureTimeout = 30 * time.Second
	dbs, err := storage.OpenDatabases(context.Background(), cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	if err = dbs.StartReplication(context.Background()); err != nil {
		t.Fatal(err)
	}
	completeCoreSeeds(t, dbs)
	f.SetReadOnly(t, "primary", storage.MainDomain, true)
	waitCore(t, dbs, func(m map[storage.Domain]coreMember) bool { return controlValue(m[storage.MainDomain].Freeze) })
	if err = dbs.StopReplication(); err != nil {
		t.Fatal(err)
	}
	f.SetReadOnly(t, "primary", storage.MainDomain, false)
	if err = dbs.Close(); err != nil {
		t.Fatal(err)
	}
	main, err := storage.OpenActiveDomain(context.Background(), cfg, storage.MainDomain, dir)
	if main != nil {
		main.Close()
	}
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal("offline writer reopened a source beyond its durable freeze", err)
	}
	history, err := storage.OpenActiveDomain(context.Background(), cfg, storage.HistoryDomain, dir)
	if err != nil {
		t.Fatal("main freeze blocked healthy history", err)
	}
	defer history.Close()
	haWrite(t, history, func(*storage.Tx) {})
}

func TestMySQLOfflineMainAllowsHealthyManualAuthorityWithoutFreeze(t *testing.T) {
	f := storagetest.Start(t)
	cfg, dir := coreConfig(t, f)
	primaryProxy, primary := storagetest.ProxyEndpoint(t, cfg.Primary)
	standbyProxy, standby := storagetest.ProxyEndpoint(t, *cfg.Standby)
	cfg.Primary = primary
	cfg.Standby = &standby
	cfg.Failover.ManualAfter = 2 * time.Second
	dbs, err := storage.OpenDatabases(context.Background(), cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	if err = dbs.StartReplication(context.Background()); err != nil {
		t.Fatal(err)
	}
	completeCoreSeeds(t, dbs)
	standbyProxy.Pause()
	primaryProxy.Pause()
	waitCore(t, dbs, func(m map[storage.Domain]coreMember) bool { return m[storage.MainDomain].Manual })
	primaryProxy.Resume()
	waitCore(t, dbs, func(m map[storage.Domain]coreMember) bool {
		return dbs.Main.Ready() && m[storage.MainDomain].Manual && !controlValue(m[storage.MainDomain].Freeze)
	})
	if err = dbs.Close(); err != nil {
		t.Fatal(err)
	}
	main, err := storage.OpenActiveDomain(context.Background(), cfg, storage.MainDomain, dir)
	if err != nil {
		t.Fatal("healthy manual-required authority was incorrectly frozen", err)
	}
	defer main.Close()
	haWrite(t, main, func(*storage.Tx) {})
}

func TestMySQLOfflineWritesRejectUnfinishedSwitchIntent(t *testing.T) {
	f := storagetest.Start(t)
	cfg, dir := coreConfig(t, f)
	primaryProxy, primary := storagetest.ProxyEndpoint(t, cfg.Primary)
	cfg.Primary = primary
	dbs, err := storage.OpenDatabases(context.Background(), cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(context.Background(), "INSERT INTO settings VALUES('public_page_enabled','false')"); err != nil {
			t.Fatal(err)
		}
	})
	if err = dbs.StartReplication(context.Background()); err != nil {
		t.Fatal(err)
	}
	completeCoreSeeds(t, dbs)
	quiesce, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = storage.ReplicaTestQuiesceAtAcknowledgedPrefix(quiesce, dbs, primaryProxy.Pause)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if err = dbs.StartReplication(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitFailover(t, dbs, "standby")
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(context.Background(), "UPDATE settings SET value='invalid' WHERE `key`='public_page_enabled'"); err != nil {
			t.Fatal(err)
		}
	})
	managementService := management.NewService(dbs.Main)
	dbs.SetMainSwitchHooks(storage.SwitchHooks{Publish: func(_ context.Context, lease *storage.Lease) error {
		_, err := managementService.PublicPageSettings(lease.Context())
		return err
	}})
	primaryProxy.Resume()
	waitCore(t, dbs, func(m map[storage.Domain]coreMember) bool {
		return m[storage.MainDomain].Active == "primary" && controlValue(m[storage.MainDomain].Intent) && !controlValue(m[storage.MainDomain].Freeze)
	})
	if err = dbs.Close(); err != nil {
		t.Fatal(err)
	}
	main, err := storage.OpenActiveDomain(context.Background(), cfg, storage.MainDomain, dir)
	if main != nil {
		main.Close()
	}
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal("offline writer bypassed the unpublished authority decision", err)
	}
}

func TestMySQLRegisteredHistorySchemaFailureDoesNotAbortHealthyMain(t *testing.T) {
	f := storagetest.Start(t)
	cfg, dir := coreConfig(t, f)
	dbs, err := storage.OpenDatabases(context.Background(), cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = dbs.Close(); err != nil {
		t.Fatal(err)
	}
	table := storage.Tables(storage.HistoryDomain)[0].Name
	if _, err = f.RawEndpoint(t, "primary", storage.HistoryDomain).Exec("DROP TABLE `" + table + "`"); err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.OpenDatabases(context.Background(), cfg, dir)
	if recovered != nil {
		defer recovered.Close()
	}
	if err != nil {
		t.Fatal("history schema error aborted the healthy main recovery process", err)
	}
	if !recovered.Main.Ready() || recovered.History.Ready() || recovered.History.Generation() != 0 {
		t.Fatal("failed history was admitted or healthy main was stopped")
	}
	history := coreMembers(t, recovered)[storage.HistoryDomain]
	if !history.Manual || history.State != "manual_required" {
		t.Fatal("history schema failure was not durably isolated", history)
	}
	haWrite(t, recovered.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(context.Background(), "INSERT INTO settings VALUES('main-survives-history','yes')"); err != nil {
			t.Fatal(err)
		}
	})
}
