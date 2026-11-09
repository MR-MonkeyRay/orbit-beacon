package history

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestHistoryStaleDeletionSetIsDiscardedBeforeSample(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, main, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.mainGeneration = main.Generation() + 1
	s.deleted = map[string]bool{"revived": true}
	s.add(Sample{"revived", now.Add(-time.Minute), map[string]float64{"cpu_percent": 7}})
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	var total float64
	if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT sum(total) FROM buckets WHERE step=60").Scan(&total); err != nil || total != 7 {
		t.Fatalf("stale deletion set rejected current sample: %v %v", total, err)
	}
}

func TestHistoryGenerationReloadsPathsAndMergesPending(t *testing.T) {
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	var stored []Bucket
	for i := range 15 {
		stored = append(stored, Bucket{Node: "node", Series: fmt.Sprintf("disk_used_percent:/stored-%02d", i), T: now.Add(-time.Minute).Unix(), Total: 1, Peak: 1, Samples: 1})
	}
	if _, err := real.Write(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	s.pending[bucketKey{"node", now.Unix()}] = map[string]aggregate{"disk_used_percent:/pending": {2, 2, 1}}
	s.paths["node"] = map[string]struct{}{"/discarded-branch": {}}
	s.pathGeneration = s.Database().Generation() + 1
	if s.admitPath("node", "/new") || len(s.paths["node"]) != 16 {
		t.Fatalf("current inventory and pending did not share quota: %v", s.paths)
	}
	if !s.admitPath("node", "/pending") {
		t.Fatal("pending path lost its admission")
	}
	if _, exists := s.paths["node"]["/discarded-branch"]; exists {
		t.Fatal("old generation path cache survived")
	}
}

func TestHistoryGenerationReplaysTombstonesAndTTLBeforeFlush(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, main, Options{})
	now := time.Now().UTC().Truncate(time.Hour)
	insertTestTombstone(t, main, "deleted", now)
	if _, err := s.cleanup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	// 候选历史可能仍包含旧墓碑已清掉的行及过期的小时桶。
	if _, err := real.Write(t.Context(), []Bucket{{Node: "deleted", Series: "cpu_percent", T: now.Add(-time.Minute).Unix(), Total: 3, Peak: 3, Samples: 1}, {Node: "expired", Series: "cpu_percent", T: now.Add(-Retention1h - time.Hour).Unix(), Total: 4, Peak: 4, Samples: 1}}); err != nil {
		t.Fatal(err)
	}
	s.historyGeneration = s.Database().Generation() + 1
	s.pathGeneration = s.historyGeneration
	s.add(Sample{"live", now.Add(-time.Minute), map[string]float64{"cpu_percent": 7}})
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT count(*) FROM buckets WHERE node_id IN ('deleted','expired')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("candidate retained deleted/expired data: %d %v", count, err)
	}
	if s.historyGeneration != s.Database().Generation() {
		t.Fatal("prepared generation not recorded")
	}
}

func TestHistoryTombstoneExpiryKeepsLastIntersectingHour(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, main, Options{})
	now := time.Now().UTC().Truncate(time.Hour)
	cutoff := now.Add(-Retention1h)
	writer, err := main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := writer.BeginWrite(writer.Context())
	if err != nil {
		writer.Close()
		t.Fatal(err)
	}
	for i := range 1002 {
		id, at := fmt.Sprintf("old-%04d", i), cutoff.Add(-time.Nanosecond)
		if i == 1001 {
			id, at = "boundary", cutoff
		}
		if _, err := tx.ExecContext(writer.Context(), "INSERT INTO node_deletions(node_id,deleted_at) VALUES(?,?)", id, timekey.Format(at)); err != nil {
			tx.Rollback()
			writer.Close()
			t.Fatal(err)
		}
	}
	err = tx.Commit()
	writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cleanupTombstones(t.Context(), now.Add(59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	lease, err := main.Acquire(t.Context(), storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	err = lease.QueryRowContext(lease.Context(), "SELECT count(*) FROM node_deletions").Scan(&count)
	lease.Close()
	if err != nil || count != 1 {
		t.Fatalf("hour boundary tombstone removed too soon: %d %v", count, err)
	}
	if err := s.cleanupTombstones(t.Context(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryFailedRealDeleteRetainsPendingWithoutRewriting(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t)
	s := stoppedStoreService(t, real, main, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"deleted", now.Add(-time.Minute), diskValues("/pending")})
	insertTestTombstone(t, main, "deleted", now)
	raw := observeStore(t, real.(*sqliteStore))
	if _, err := raw.Exec("INSERT INTO buckets VALUES('deleted','cpu_percent',60,?,1,1,1)", now.Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TRIGGER reject_delete BEFORE DELETE ON buckets BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cleanup(t.Context(), now); err == nil {
		t.Fatal("injected delete succeeded")
	}
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := raw.QueryRow("SELECT count(*) FROM buckets").Scan(&count); err != nil || count != 1 || len(s.pending) != 1 || len(s.paths["deleted"]) != 1 {
		t.Fatalf("failed deletion consumed or rewrote private data: rows=%d pending=%v paths=%v err=%v", count, s.pending, s.paths, err)
	}
}

func TestHistoryDeletionBatchRechecksRevivalAndReleasesMain(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t).(*sqliteStore)
	s := stoppedStoreService(t, real, main, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	insertTestTombstone(t, main, "revived", now)
	raw := observeStore(t, real)
	if _, err := raw.Exec("WITH RECURSIVE n(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM n WHERE v<2001) INSERT INTO buckets SELECT 'revived','cpu_percent',60,?-v,1,1,1 FROM n", now.Unix()); err != nil {
		t.Fatal(err)
	}
	batches := 0
	removed, err := real.DeleteNode(t.Context(), "revived", func() {
		batches++
		if batches != 1 {
			return
		}
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
		if _, err := tx.ExecContext(lease.Context(), "INSERT INTO nodes(id,name,created_at) VALUES('revived','revived',?)", timekey.Format(now)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil || removed != 1000 {
		t.Fatalf("deletion crossed revival: removed=%d batches=%d err=%v", removed, batches, err)
	}
	if err := main.Close(); err != nil {
		t.Fatal(err)
	}
	// 真实 Close 必须在批次已返回时完成，不能留下历史删除的 main lease。
	if _, err := main.Acquire(t.Context(), storage.Reader); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("closed main admitted SQL: %v", err)
	}
	_ = s
}

func TestHistoryGeneration8192LongestPathsExactlyOnce(t *testing.T) {
	real := openTestSQLiteStore(t)
	var batches []int
	s := stoppedStoreService(t, real, nil, Options{ObserveFlush: func(_ time.Duration, rows int) { batches = append(batches, rows) }})
	now := time.Now().UTC().Truncate(time.Minute)
	for i := range 8192 {
		node := fmt.Sprintf("00000000-0000-4000-8000-%012d", i%128)
		at := now.Add(-time.Duration(1+i/128) * time.Minute)
		s.add(Sample{node, at, map[string]float64{"cpu_percent": 1, "load1": 2, "disk_used_percent:" + strings.Repeat("x", 1024): 3, "inode_used_percent:" + strings.Repeat("y", 1024): 4}})
	}
	s.historyGeneration = s.Database().Generation() + 1
	s.pathGeneration = s.historyGeneration
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 64 || len(s.pending) != 0 || len(s.inflight) != 0 {
		t.Fatalf("bounded backlog not drained: batches=%d pending=%d inflight=%d", len(batches), len(s.pending), len(s.inflight))
	}
	for _, rows := range batches {
		if rows != 512*3 {
			t.Fatalf("unexpected batch rows=%d", rows)
		}
	}
	for _, step := range []int{60, 300, 3600} {
		var total float64
		var samples int64
		if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE step=?", step).Scan(&total, &samples); err != nil || total != 81920 || samples != 32768 {
			t.Fatalf("step=%d total=%v samples=%d err=%v", step, total, samples, err)
		}
	}
}
