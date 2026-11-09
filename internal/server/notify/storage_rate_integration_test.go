package notify

import (
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestMySQLStorageSnapshotImportsSQLRateFloorBeforePublishing(t *testing.T) {
	s, dbs, c, _ := outageServiceFixture(t)
	now := time.Now().UTC()
	s.opts.Now = func() time.Time { return now }
	lease, err := dbs.Main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = s.pauseStorageSnapshot(lease.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(lease.Context(), `INSERT INTO notification_rate_limits(channel_id,window_start,count) VALUES(?,?,?)`, c.ID, timekey.Format(now), 20); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err = s.SyncStorageSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := dbs.OutageSnapshot(t.Context())
	if err != nil || !snapshot.Ready {
		t.Fatal("validated snapshot did not publish", err)
	}
	allowed, _, configured, err := dbs.ReserveNotificationRate(t.Context(), c.ID, now)
	if err != nil || !configured || allowed {
		t.Fatal("new outage quota ignored existing SQL sends", allowed, configured, err)
	}
}

func TestMySQLMalformedRateWindowDoesNotMutateControlQuota(t *testing.T) {
	s, dbs, c, _ := outageServiceFixture(t)
	now := time.Now().UTC()
	s.opts.Now = func() time.Time { return now }
	lease, err := dbs.Main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(lease.Context(), `INSERT INTO notification_rate_limits(channel_id,window_start,count) VALUES(?,?,?)`, c.ID, "invalid", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.reserveRate(lease.Context(), tx, c.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatal("malformed HA window accepted", err)
	}
	for range 20 {
		allowed, _, configured, err := dbs.ReserveNotificationRate(t.Context(), c.ID, now)
		if err != nil || !configured || !allowed {
			t.Fatal("rejected window consumed quota", allowed, configured, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.SyncStorageSnapshot(t.Context()); err != nil {
		t.Fatal("malformed snapshot rate blocked healthy main", err)
	}
	snapshot, err := dbs.OutageSnapshot(t.Context())
	if err != nil || snapshot.Ready {
		t.Fatal("malformed rate published external snapshot", err)
	}
}

func TestStandaloneMalformedRateWindowKeepsExistingResetBehavior(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(`INSERT INTO notification_rate_limits(channel_id,window_start,count) VALUES(?,?,?)`, f.channel.ID, "invalid", 0); err != nil {
		t.Fatal(err)
	}
	for _, databases := range []*storage.Databases{nil, {Main: f.s.db}} {
		f.s.opts.Storage = databases
		lease, err := f.s.db.Acquire(t.Context(), storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			lease.Close()
			t.Fatal(err)
		}
		allowed, _, err := f.s.reserveRate(lease.Context(), tx, f.channel.ID)
		tx.Rollback()
		lease.Close()
		if err != nil || !allowed {
			t.Fatal("standalone malformed window no longer reset", allowed, err)
		}
	}
}
