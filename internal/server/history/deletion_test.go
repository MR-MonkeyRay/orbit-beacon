package history

import (
	"context"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func insertTestTombstone(t *testing.T, main *storage.Database, id string, at time.Time) {
	t.Helper()
	lease, err := main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(lease.Context(), "INSERT INTO node_deletions(node_id,deleted_at) VALUES(?,?)", id, timekey.Format(at)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryMissingCreationDoesNotDeleteHistory(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, main, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"lost-creation", now.Add(-time.Minute), diskValues("/retained")})
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cleanup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT count(*) FROM buckets WHERE node_id='lost-creation'").Scan(&count); err != nil || count != 12 {
		t.Fatalf("missing creation erased history: count=%d err=%v", count, err)
	}
}

func TestHistoryTombstoneRejectsLateSamplesAndRevivalAcceptsOnlyNew(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, main, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"deleted", now.Add(-time.Minute), diskValues("/old")})
	insertTestTombstone(t, main, "deleted", now)
	if _, err := s.cleanup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	s.add(Sample{"deleted", now.Add(-time.Minute), diskValues("/late")})
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	var count int
	raw := observeStore(t, real.(*sqliteStore))
	if err := raw.QueryRow("SELECT count(*) FROM buckets").Scan(&count); err != nil || count != 0 {
		t.Fatalf("late report recreated deleted history: %d %v", count, err)
	}
	lease, err := main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(lease.Context(), "INSERT INTO nodes(id,name,created_at) VALUES('deleted','revived',?)", timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if _, err := s.cleanup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	s.add(Sample{"deleted", now.Add(-time.Minute), map[string]float64{"cpu_percent": 7}})
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	var total float64
	if err := raw.QueryRow("SELECT sum(total) FROM buckets WHERE step=60").Scan(&total); err != nil || total != 7 {
		t.Fatalf("revival replayed old pending or rejected new data: %v %v", total, err)
	}
}

func TestHistoryExpiredPendingCannotRegenerateRollups(t *testing.T) {
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, nil, Options{})
	now := time.Now().UTC().Truncate(time.Hour)
	s.add(Sample{"node", now.Add(-Retention1m - time.Minute), map[string]float64{"cpu_percent": 9}})
	if err := s.flush(context.Background(), now, true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT count(*) FROM buckets").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired pending regenerated rollups: %d %v", count, err)
	}
}
