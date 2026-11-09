package history

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLHistoryResolvedReceiptCrossesGenerationWithoutLease(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
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
			store := &sqliteStore{db: dbs.History}
			s := stoppedStoreService(t, store, nil, Options{})
			now := time.Now().UTC().Truncate(time.Minute)
			buckets := []Bucket{{Node: "node", Series: "cpu_percent", T: now.Add(-time.Minute).Unix(), Total: 20, Peak: 20, Samples: 1}}
			lease, err := dbs.History.Acquire(ctx, storage.Writer)
			if err != nil {
				t.Fatal(err)
			}
			generation := lease.Generation()
			tx, err := lease.BeginWrite(lease.Context())
			if err != nil {
				lease.Close()
				t.Fatal(err)
			}
			query, args, _, err := mysqlHistoryBatch(buckets, 0)
			if err == nil {
				_, err = tx.ExecContext(lease.Context(), query, args...)
			}
			if err != nil {
				tx.Rollback()
				lease.Close()
				t.Fatal(err)
			}
			if committed {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			lease.Close()
			if err != nil {
				t.Fatal(err)
			}
			// 原收据已解析，消费者尚未消费它；不能持有退役 lease 等待世代发布。
			store.unresolved, store.inflight, s.inflight = tx, slices.Clone(buckets), slices.Clone(buckets)
			original := slices.Clone(s.inflight)
			source, target := fixture.RawEndpoint(t, "primary", storage.HistoryDomain), fixture.RawEndpoint(t, "standby", storage.HistoryDomain)
			waitHistoryCondition(t, "history has no acknowledged barrier", func() bool { return historyReplicaCaughtUp(source, target) })
			proxy.Pause()
			defer proxy.Resume()
			waitHistoryCondition(t, "unconsumed receipt prevented generation drain", func() bool { return dbs.History.Generation() > generation && dbs.History.Ready() })
			if !slices.Equal(s.inflight, original) {
				t.Fatal("generation publication mutated immutable inflight data")
			}
			s.add(Sample{"node", now.Add(-time.Minute), map[string]float64{"cpu_percent": 30}})
			if err := s.flush(ctx, now, false); err != nil {
				t.Fatal(err)
			}
			if store.unresolved != nil || len(s.inflight) != 0 || len(s.pending) != 0 {
				t.Fatal("new generation retained resolved receipt or unflushed samples")
			}
			for _, step := range []int{60, 300, 3600} {
				var total float64
				var samples int64
				if err := target.QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE node_id='node' AND step=?", step).Scan(&total, &samples); err != nil || total != 50 || samples != 2 {
					t.Fatalf("resolved receipt duplicated or lost data: step=%d total=%v samples=%d err=%v", step, total, samples, err)
				}
			}
		})
	}
}
