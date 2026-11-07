package storage_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLReplicaJournalBudgetCancelsPinnedSnapshot(t *testing.T) {
	f := storagetest.Start(t)
	ctx := context.Background()
	standby := f.Endpoint("standby")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbs, err := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	canceled, release, err := storage.ReplicaTestHoldSnapshot(ctx, dbs, storage.MainDomain)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	type member struct {
		Ready bool
		State string
	}
	type metadata struct {
		Revision uint64
		Domains  map[storage.Domain]member
	}
	readMetadata := func() metadata {
		raw, err := dbs.CaptureRestoreMetadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m metadata
		if err = json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	before := readMetadata()
	value := strings.Repeat("x", 128<<10)
	// 每笔约 60 MiB，实际捕获总量超过 256 MiB；不改协议计数制造压力。
	for batch := range 5 {
		haWrite(t, dbs.Main, func(tx *storage.Tx) {
			for row := range 240 {
				query := "UPDATE settings SET value=? WHERE `key`='budget'"
				if batch == 0 && row == 0 {
					query = "INSERT INTO settings VALUES('budget',?)"
				}
				if _, err := tx.ExecContext(ctx, query, value); err != nil {
					t.Fatal(err)
				}
			}
		})
		if batch < 4 && readMetadata().Revision != before.Revision {
			t.Fatal("ordinary commits persisted local control intents")
		}
	}
	select {
	case <-canceled:
	default:
		t.Fatal("source exceeded journal budget without canceling the snapshot")
	}
	_, pinned := storage.ReplicaTestSnapshotPinned(dbs, storage.MainDomain)
	if pinned {
		t.Fatal("canceled snapshot retained its log pin")
	}
	after := readMetadata()
	if m := after.Domains[storage.MainDomain]; m.Ready || m.State != "rebuild_required" {
		t.Fatalf("qualification not durably revoked: %+v", m)
	}
	raw := f.RawEndpoint(t, "primary", storage.MainDomain)
	var bytes, sum, head, retained int64
	if err = raw.QueryRow("SELECT journal_bytes,(SELECT COALESCE(SUM(payload_bytes),0) FROM replica_transactions),head_seq,retained_from_seq FROM replica_state").Scan(&bytes, &sum, &head, &retained); err != nil {
		t.Fatal(err)
	}
	if bytes > 256<<20 || bytes != sum || head != 5 || retained <= 1 {
		t.Fatalf("unbounded or inconsistent journal bytes=%d sum=%d head=%d retained=%d", bytes, sum, head, retained)
	}
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value='still-healthy' WHERE `key`='budget'"); err != nil {
			t.Fatal(err)
		}
	})
	if !dbs.Main.Ready() {
		t.Fatal("offline standby pressure stopped healthy source")
	}
}
