package storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLReplicaTargetConstraintsRevokeWithoutRetry(t *testing.T) {
	for _, sqliteTarget := range []bool{false, true} {
		for _, kind := range []string{"primary_key", "unique", "foreign_key"} {
			t.Run(fmt.Sprintf("sqlite=%v/%s", sqliteTarget, kind), func(t *testing.T) {
				ctx := context.Background()
				f := storagetest.Start(t)
				standby := f.Endpoint("standby")
				if sqliteTarget {
					standby = config.DatabaseEndpoint{Driver: "sqlite"}
				}
				dir := t.TempDir()
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				dbs, err := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}, dir)
				if err != nil {
					t.Fatal(err)
				}
				defer dbs.Close()
				source := f.RawEndpoint(t, "primary", storage.MainDomain)
				if kind == "foreign_key" {
					haWrite(t, dbs.Main, func(tx *storage.Tx) {
						if _, err := tx.ExecContext(ctx, "INSERT INTO `groups`(id,name) VALUES('parent','parent')"); err != nil {
							t.Fatal(err)
						}
					})
				}
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
				if err = dbs.StartReplication(ctx); err != nil {
					t.Fatal(err)
				}
				waitReplica(t, source, target)
				if err = dbs.StopReplication(); err != nil {
					t.Fatal(err)
				}
				var before int64
				if err = target.QueryRow("SELECT head_seq FROM replica_state WHERE id=1").Scan(&before); err != nil {
					t.Fatal(err)
				}
				mutation, write := "INSERT INTO `groups`(id,name) VALUES('conflict','foreign')", "INSERT INTO `groups`(id,name) VALUES('conflict','source')"
				if kind == "unique" {
					mutation, write = "INSERT INTO `groups`(id,name) VALUES('foreign','duplicate')", "INSERT INTO `groups`(id,name) VALUES('source','duplicate')"
				}
				if kind == "foreign_key" {
					mutation, write = "DELETE FROM `groups` WHERE id='parent'", "INSERT INTO nodes(id,name,group_id,created_at) VALUES('source','source','parent','now')"
				}
				if _, err = target.ExecContext(ctx, mutation); err != nil {
					t.Fatal(err)
				}
				haWrite(t, dbs.Main, func(tx *storage.Tx) {
					for _, query := range []string{"INSERT INTO settings VALUES('must-rollback','source')", write} {
						if _, err := tx.ExecContext(ctx, query); err != nil {
							t.Fatal(err)
						}
					}
				})
				var failedID string
				if err = source.QueryRow("SELECT head_tx_uuid FROM replica_state WHERE id=1").Scan(&failedID); err != nil {
					t.Fatal(err)
				}
				if err = dbs.StartReplication(ctx); err != nil {
					t.Fatal(err)
				}
				type member struct {
					State string
					Ready bool
				}
				read := func() member {
					raw, err := dbs.CaptureRestoreMetadata(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var metadata struct{ Domains map[storage.Domain]member }
					if err = json.Unmarshal(raw, &metadata); err != nil {
						t.Fatal(err)
					}
					return metadata.Domains[storage.MainDomain]
				}
				deadline := time.Now().Add(4 * time.Second)
				state := read()
				for time.Now().Before(deadline) && state.State != "manual_required" {
					time.Sleep(20 * time.Millisecond)
					state = read()
				}
				if state.Ready || state.State != "manual_required" {
					t.Errorf("deterministic target conflict remained retryable/qualified: %+v", state)
				}
				var after, count int64
				if err = target.QueryRow("SELECT head_seq FROM replica_state WHERE id=1").Scan(&after); err != nil {
					t.Fatal(err)
				}
				if after != before {
					t.Errorf("conflict advanced checkpoint: %d -> %d", before, after)
				}
				for _, query := range []string{"SELECT count(*) FROM settings WHERE `key`='must-rollback'", "SELECT count(*) FROM replica_transactions WHERE tx_uuid=?"} {
					args := []any(nil)
					if query[len(query)-1] == '?' {
						args = []any{failedID}
					}
					if err = target.QueryRow(query, args...).Scan(&count); err != nil || count != 0 {
						t.Fatal("partial transaction survived", count, err)
					}
				}
				time.Sleep(1100 * time.Millisecond)
				if final := read(); final.State != "manual_required" || final.Ready {
					t.Error("conflict silently resumed", final)
				}
				if err = target.QueryRow("SELECT head_seq FROM replica_state WHERE id=1").Scan(&after); err != nil || after != before {
					t.Fatal("stopped conflict advanced checkpoint", after, err)
				}
			})
		}
	}
}
