package storage_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func failoverState(t *testing.T, dbs *storage.Databases, domain storage.Domain) (string, bool) {
	t.Helper()
	raw, err := dbs.CaptureRestoreMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Domains map[storage.Domain]struct {
			Active string
			Ready  bool
		}
	}
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	m := record.Domains[domain]
	return m.Active, m.Ready
}
func awaitFailover(t *testing.T, dbs *storage.Databases, role string) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		active, _ := failoverState(t, dbs, storage.MainDomain)
		if active == role && dbs.Main.Ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := dbs.CaptureRestoreMetadata(context.Background())
	t.Fatalf("authority did not converge to %s: %s", role, raw)
}

func TestMySQLFailoverDiscardsOldTailAndFailsBackAfterStableCatchup(t *testing.T) {
	f := storagetest.Start(t)
	primaryProxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
	standbyProxy, standby := storagetest.ProxyEndpoint(t, f.Endpoint("standby"))
	cfg := config.DatabaseConfig{Primary: primary, Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: 2 * time.Second, ManualAfter: time.Hour}}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	stableMain, stableHistory := dbs.Main, dbs.History
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('delete-me','old')"); err != nil {
			t.Fatal(err)
		}
	})
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	source, target := f.RawEndpoint(t, "primary", storage.MainDomain), f.RawEndpoint(t, "standby", storage.MainDomain)
	waitReplica(t, source, target)
	// seed checkpoint 不是事务屏障；至少复制一次 seed 后真实 guard 事务才具有 B 证明。
	haWrite(t, dbs.Main, func(*storage.Tx) {})
	waitReplica(t, source, target)
	var barriers int
	if err = target.QueryRow("SELECT count(*) FROM replica_transactions t JOIN replica_state s ON t.epoch_uuid=s.epoch_uuid AND t.seq=s.head_seq WHERE s.id=1").Scan(&barriers); err != nil || barriers != 1 {
		t.Fatal("target has no transaction barrier after seed", barriers, err)
	}
	standbyProxy.Pause()
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('discard-tail','old-only')"); err != nil {
			t.Fatal(err)
		}
	})
	firstGeneration := dbs.Main.Generation()
	primaryProxy.Pause()
	standbyProxy.Resume()
	awaitFailover(t, dbs, "standby")
	if dbs.Main != stableMain || dbs.History != stableHistory || dbs.Main.Generation() <= firstGeneration {
		t.Fatal("switch replaced stable handles or reused generation")
	}
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('standby-new','preserved')"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE `key`='delete-me'"); err != nil {
			t.Fatal(err)
		}
	})
	var count int
	if err = source.QueryRow("SELECT count(*) FROM settings WHERE `key`='discard-tail'").Scan(&count); err != nil || count != 1 {
		t.Fatal("fixture did not preserve a real old-primary unique tail", count, err)
	}
	secondGeneration := dbs.Main.Generation()
	resumed := time.Now()
	primaryProxy.Resume()
	awaitFailover(t, dbs, "primary")
	if time.Since(resumed) < cfg.Failover.RecoveryStable {
		t.Fatal("failed back before continuous writeable stability")
	}
	if dbs.Main.Generation() <= secondGeneration {
		t.Fatal("failback reused generation")
	}
	for _, raw := range []struct {
		name string
		key  string
		want int
	}{{"discard", "discard-tail", 0}, {"new", "standby-new", 1}, {"delete", "delete-me", 0}} {
		if err = source.QueryRow("SELECT count(*) FROM settings WHERE `key`=?", raw.key).Scan(&count); err != nil || count != raw.want {
			t.Fatalf("%s final rows=%d want=%d err=%v", raw.name, count, raw.want, err)
		}
	}
}
