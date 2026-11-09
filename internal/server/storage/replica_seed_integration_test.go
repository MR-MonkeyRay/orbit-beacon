package storage_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLReplicationSeedRRConcurrentWritesAndRestart(t *testing.T) {
	for _, sqliteTarget := range []bool{false, true} {
		t.Run(fmt.Sprint("sqlite=", sqliteTarget), func(t *testing.T) {
			f := storagetest.Start(t)
			ctx := context.Background()
			standby := f.Endpoint("standby")
			if sqliteTarget {
				standby = config.DatabaseEndpoint{Driver: "sqlite"}
			}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}
			dbs, err := storage.OpenDatabases(ctx, cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if dbs != nil {
					dbs.Close()
				}
			}()
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				var values []string
				var args []any
				for i := range 1024 {
					values = append(values, "(?,?)")
					args = append(args, fmt.Sprintf("seed-%04d", i), "base")
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO settings(`key`,value) VALUES "+strings.Join(values, ","), args...); err != nil {
					t.Fatal(err)
				}
			})
			source := f.RawEndpoint(t, "primary", storage.MainDomain)
			var target *sql.DB
			if sqliteTarget {
				target, err = storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer target.Close()
			} else {
				target = f.RawEndpoint(t, "standby", storage.MainDomain)
			}
			block := func() func() {
				tx, err := target.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				query := "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1"
				if !sqliteTarget {
					query += " FOR UPDATE"
				}
				var id string
				if err = tx.QueryRowContext(ctx, query).Scan(&id); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				return func() { tx.Rollback() }
			}
			waitPin := func() int64 {
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) {
					seq, pinned := storage.ReplicaTestSnapshotPinned(dbs, storage.MainDomain)
					if pinned {
						return seq
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("seed did not establish its guarded RR snapshot")
				return 0
			}
			release := block()
			defer release()
			if err = dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			first := waitPin()
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				if _, err := tx.ExecContext(ctx, "UPDATE settings SET value='before-cancel' WHERE `key`='seed-0000'"); err != nil {
					t.Fatal(err)
				}
			})
			var sourceHead int64
			if err = source.QueryRow("SELECT head_seq FROM replica_state").Scan(&sourceHead); err != nil || sourceHead <= first {
				t.Fatal("write did not follow pinned snapshot", sourceHead, first, err)
			}
			if err = dbs.StopReplication(); err != nil {
				t.Fatal(err)
			}
			release()
			var ready bool
			if err = target.QueryRow("SELECT ready FROM replica_state").Scan(&ready); err != nil || ready {
				t.Fatalf("canceled seed became ready: %v %v", ready, err)
			}
			if err = dbs.Close(); err != nil {
				t.Fatal(err)
			}
			dbs, err = storage.OpenDatabases(ctx, cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			releaseAgain := block()
			defer releaseAgain()
			if err = dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			second := waitPin()
			// 目标 guard 保持关闭，三种修改必然发生在同一个源 RR 快照建立之后。
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				for _, query := range []string{"UPDATE settings SET value='final' WHERE `key`='seed-0000'", "DELETE FROM settings WHERE `key`='seed-0001'", "INSERT INTO settings VALUES('seed-new','after-snapshot')"} {
					if _, err := tx.ExecContext(ctx, query); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err = source.QueryRow("SELECT head_seq FROM replica_state").Scan(&sourceHead); err != nil || sourceHead <= second {
				t.Fatal("concurrent writes did not follow restart snapshot", sourceHead, second, err)
			}
			releaseAgain()
			waitReplica(t, source, target)
			var count, deleted int
			var value, newValue string
			if err = target.QueryRow("SELECT count(*),(SELECT count(*) FROM settings WHERE `key`='seed-0001') FROM settings").Scan(&count, &deleted); err != nil || count != 1024 || deleted != 0 {
				t.Fatalf("seed/replay row set count=%d deleted=%d err=%v", count, deleted, err)
			}
			if err = target.QueryRow("SELECT value FROM settings WHERE `key`='seed-0000'").Scan(&value); err != nil || value != "final" {
				t.Fatal("snapshot overwrote concurrent update", value, err)
			}
			if err = target.QueryRow("SELECT value FROM settings WHERE `key`='seed-new'").Scan(&newValue); err != nil || newValue != "after-snapshot" {
				t.Fatal("concurrent insert lost", newValue, err)
			}
		})
	}
}
