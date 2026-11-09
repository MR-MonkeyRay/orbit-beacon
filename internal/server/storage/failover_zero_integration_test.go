package storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLZeroLossPromotionIncludesAllPossibleCommits(t *testing.T) {
	for _, driver := range []string{"mysql", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			f := storagetest.Start(t)
			ctx := context.Background()
			primaryProxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
			standby := f.Endpoint("standby")
			if driver == "sqlite" {
				standby = config.DatabaseEndpoint{Driver: "sqlite"}
			}
			cfg := config.DatabaseConfig{Primary: primary, Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 0, RecoveryStable: time.Hour, ManualAfter: 24 * time.Hour}}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			dbs, err := storage.OpenDatabases(ctx, cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer dbs.Close()
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('zero-loss-prefix','kept')"); err != nil {
					t.Fatal(err)
				}
			})
			if err = dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			type member struct {
				Active, State, Reason string
				Ready                 bool
				Rebuild               json.RawMessage
				LossNanos             *int64
			}
			state := func(domain storage.Domain) member {
				raw, err := dbs.CaptureRestoreMetadata(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var record struct{ Domains map[storage.Domain]member }
				if err = json.Unmarshal(raw, &record); err != nil {
					t.Fatal(err)
				}
				return record.Domains[domain]
			}
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				main, history := state(storage.MainDomain), state(storage.HistoryDomain)
				if main.Ready && history.Ready && string(main.Rebuild) == "null" && string(history.Rebuild) == "null" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !state(storage.MainDomain).Ready || !state(storage.HistoryDomain).Ready {
				t.Fatal("both standby seeds must publish complete readiness")
			}
			var target *sql.DB
			if driver == "sqlite" {
				target, err = storage.OpenReader(ctx, filepath.Join(dir, "orbitbeacon.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer target.Close()
			} else {
				target = f.RawEndpoint(t, "standby", storage.MainDomain)
			}
			haWrite(t, dbs.Main, func(*storage.Tx) {})
			haWrite(t, dbs.History, func(*storage.Tx) {})
			mainGeneration, historyGeneration := dbs.Main.Generation(), dbs.History.Generation()
			quiesce, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = storage.ReplicaTestQuiesceAtAcknowledgedPrefix(quiesce, dbs, primaryProxy.Pause)
			cancel()
			if err != nil {
				t.Fatal("could not establish a real fully acknowledged prefix before network loss", err)
			}
			if err = dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(40 * time.Second)
			for time.Now().Before(deadline) {
				if state(storage.MainDomain).Active == "standby" && state(storage.HistoryDomain).Active == "standby" && dbs.Main.Ready() && dbs.History.Ready() {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			m := state(storage.MainDomain)
			if m.Active != "standby" || !dbs.Main.Ready() {
				t.Fatalf("complete possible prefix was not promoted with zero loss: %+v", m)
			}
			if m.LossNanos == nil || *m.LossNanos != 0 || dbs.Main.Generation() <= mainGeneration {
				t.Fatal("complete possible prefix did not publish an exact zero bound", m.LossNanos, dbs.Main.Generation())
			}
			if !dbs.History.Ready() || dbs.History.Generation() <= historyGeneration || state(storage.HistoryDomain).Active != "standby" {
				t.Fatal("complete history prefix did not independently promote")
			}
			haWrite(t, dbs.History, func(*storage.Tx) {})
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('zero-loss-new-authority','new')"); err != nil {
					t.Fatal(err)
				}
			})
			var count int
			if err = target.QueryRow("SELECT count(*) FROM settings WHERE `key` IN ('zero-loss-prefix','zero-loss-new-authority')").Scan(&count); err != nil || count != 2 {
				t.Fatal("zero-loss promotion lost or misrouted actual writes", count, err)
			}
		})
	}
}
