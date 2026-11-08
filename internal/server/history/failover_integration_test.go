package history

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func waitHistoryCondition(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(description)
}

func historyReplicaCaughtUp(source, target *sql.DB) bool {
	var sourceEpoch, sourceID, targetEpoch, targetID string
	var sourceSeq, targetSeq int64
	var ready bool
	if source.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state").Scan(&sourceEpoch, &sourceSeq, &sourceID) != nil {
		return false
	}
	if target.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid,ready FROM replica_state").Scan(&targetEpoch, &targetSeq, &targetID, &ready) != nil {
		return false
	}
	if !ready || sourceEpoch != targetEpoch || sourceSeq != targetSeq || sourceID != targetID {
		return false
	}
	var barriers int
	err := target.QueryRow("SELECT count(*) FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=? AND boot_id<>'' AND barrier_ns>0", targetEpoch, targetSeq, targetID).Scan(&barriers)
	return err == nil && barriers == 1
}

func TestMySQLHistoryPromotionPreservesPendingAndReplaysDeletion(t *testing.T) {
	fixture := storagetest.Start(t)
	proxy, primary := storagetest.ProxyEndpoint(t, fixture.Endpoint("primary"))
	standby := fixture.Endpoint("standby")
	cfg := config.DatabaseConfig{Primary: primary, Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: 60 * time.Second, ManualAfter: time.Hour}}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	if err := dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	s := OpenWithDatabase(dbs.History, dbs.Main, Options{Now: func() time.Time { return now }})
	defer func() {
		stop, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := s.Shutdown(stop); err != nil {
			t.Error(err)
		}
	}()
	samples := s.Samples()
	nodeService := nodes.NewService(dbs.Main, nodes.Options{})
	live, err := nodeService.CreatePending(ctx, nodes.CreateRequest{Name: "live"})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := nodeService.CreatePending(ctx, nodes.CreateRequest{Name: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	samples <- Sample{live.ID, now.Add(-2 * time.Minute), map[string]float64{"cpu_percent": 1, "disk_used_percent:/existing": 2}}
	samples <- Sample{deleted.ID, now.Add(-2 * time.Minute), map[string]float64{"cpu_percent": 4}}
	if _, err := s.Cleanup(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Write(ctx, []Bucket{{Node: "expired", Series: "cpu_percent", T: now.Add(-Retention1h - time.Hour).Unix(), Total: 3, Peak: 3, Samples: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := nodeService.Delete(ctx, deleted.ID, true); err != nil {
		t.Fatal(err)
	}
	primaryMain, standbyMain := fixture.RawEndpoint(t, "primary", storage.MainDomain), fixture.RawEndpoint(t, "standby", storage.MainDomain)
	primaryHistory, standbyHistory := fixture.RawEndpoint(t, "primary", storage.HistoryDomain), fixture.RawEndpoint(t, "standby", storage.HistoryDomain)
	waitHistoryCondition(t, "initial main/history checkpoints did not catch up", func() bool {
		return historyReplicaCaughtUp(primaryMain, standbyMain) && historyReplicaCaughtUp(primaryHistory, standbyHistory)
	})
	if err := dbs.StopReplication(); err != nil {
		t.Fatal(err)
	}
	// 旧主已提交但未复制的独有尾部必须丢弃，私有未提交样本则必须保留。
	samples <- Sample{live.ID, now.Add(-time.Minute), map[string]float64{"cpu_percent": 5, "disk_used_percent:/discarded": 6}}
	if _, err := s.Cleanup(ctx, now); err != nil {
		t.Fatal(err)
	}
	samples <- Sample{live.ID, now, map[string]float64{"cpu_percent": 7, "disk_used_percent:/pending": 3}}
	samples <- Sample{deleted.ID, now, map[string]float64{"cpu_percent": 11}}
	mainGeneration, historyGeneration := dbs.Main.Generation(), dbs.History.Generation()
	proxy.Pause()
	defer proxy.Resume()
	if err := dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	waitHistoryCondition(t, "main/history did not publish actual standby generations", func() bool {
		return dbs.Main.Generation() > mainGeneration && dbs.History.Generation() > historyGeneration && dbs.Main.Ready() && dbs.History.Ready()
	})
	if s.Samples() != samples {
		t.Fatal("history replaced its producer channel")
	}
	select {
	case <-s.done:
		t.Fatal("history goroutine stopped during promotion")
	default:
	}
	// 新世代首个路径样本必须先做 TTL 和墓碑重放，再合并新库存量与私有 pending。
	samples <- Sample{live.ID, now, map[string]float64{"disk_used_percent:/pending": 3}}
	waitHistoryCondition(t, "history promotion did not purge candidate tombstones and expired tiers", func() bool {
		var count int
		return standbyHistory.QueryRow("SELECT count(*) FROM buckets WHERE node_id IN (?,?)", deleted.ID, "expired").Scan(&count) == nil && count == 0
	})
	if _, err := s.Cleanup(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, stale := s.paths[live.ID]["/discarded"]; stale || len(s.paths[live.ID]) != 2 {
		t.Fatalf("old branch paths survived promotion: %v", s.paths[live.ID])
	}
	for _, step := range []int{60, 300, 3600} {
		var total float64
		var count int64
		if err := standbyHistory.QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE node_id=? AND metric='cpu_percent' AND step=?", live.ID, step).Scan(&total, &count); err != nil || total != 8 || count != 2 {
			t.Fatalf("pending lost or discarded tail replayed: step=%d total=%v samples=%d err=%v", step, total, count, err)
		}
		if err := standbyHistory.QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE node_id=? AND metric='disk_used_percent:/pending' AND step=?", live.ID, step).Scan(&total, &count); err != nil || total != 6 || count != 2 {
			t.Fatalf("pending path was not counted exactly once: step=%d total=%v samples=%d err=%v", step, total, count, err)
		}
	}
}

func TestMySQLHistoryResolvedRollbackRevalidatesTombstone(t *testing.T) {
	fixture := storagetest.Start(t)
	main := emptyTestMain(t)
	database := fixture.Open(t, storage.HistoryDomain)
	store := &sqliteStore{db: database}
	s := stoppedStoreService(t, store, main, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	buckets := []Bucket{{Node: "deleted", Series: "cpu_percent", T: now.Add(-time.Minute).Unix(), Total: 9, Peak: 9, Samples: 1}}
	lease, err := database.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	// 协调器已证明回滚，但 Service 尚未消费同一笔收据。
	store.unresolved, store.inflight, s.inflight = tx, append([]Bucket(nil), buckets...), append([]Bucket(nil), buckets...)
	insertTestTombstone(t, main, "deleted", now)
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := fixture.Raw(t, storage.HistoryDomain).QueryRow("SELECT count(*) FROM buckets WHERE node_id='deleted'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("resolved old batch rewrote a deleted node: %d %v", count, err)
	}
	if store.unresolved != nil || len(s.inflight) != 0 {
		t.Fatal("resolved receipt retained SQL ownership")
	}
}
