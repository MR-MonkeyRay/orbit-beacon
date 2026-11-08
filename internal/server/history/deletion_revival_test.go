package history

import (
	"context"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

// 装饰真实 Store，只固定 DELETE 提交与随后的 main 快照之间的时序。
// 数据删除、节点复活与后续刷写均执行真实 SQL。
type deletionBoundaryStore struct {
	Store
	beforeYield func(context.Context) error
	afterDelete func(context.Context) error
}

func (s *deletionBoundaryStore) DeleteNode(ctx context.Context, node string, yield func()) (int64, error) {
	var yieldErr error
	removed, err := s.Store.DeleteNode(ctx, node, func() {
		if yieldErr == nil && s.beforeYield != nil {
			yieldErr = s.beforeYield(ctx)
		}
		yield()
	})
	if err == nil {
		err = yieldErr
	}
	if err == nil && s.afterDelete != nil {
		err = s.afterDelete(ctx)
	}
	return removed, err
}

func TestHistorySuccessfulDeleteThenRevivalCannotReplayOldPending(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t).(*sqliteStore)
	real.nodes = main
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := real.Write(t.Context(), []Bucket{{Node: "revived", Series: "cpu_percent", T: now.Add(-2 * time.Minute).Unix(), Total: 1, Peak: 1, Samples: 1}}); err != nil {
		t.Fatal(err)
	}
	insertTestTombstone(t, main, "revived", now)
	boundary := &deletionBoundaryStore{Store: real}
	boundary.afterDelete = func(ctx context.Context) error {
		boundary.afterDelete = nil
		lease, err := main.Acquire(ctx, storage.Writer)
		if err != nil {
			return err
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(lease.Context(), "INSERT INTO nodes(id,name,created_at) VALUES('revived','revived',?)", timekey.Format(now)); err != nil {
			return err
		}
		return tx.Commit()
	}
	s := newService(boundary, main, Options{Now: func() time.Time { return now }})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	// 同一 run goroutine 先保留删除前的私有桶与路径，再执行删除及复活。
	s.Samples() <- Sample{"revived", now.Add(-time.Minute), diskValues("/before-delete")}
	if _, err := s.Cleanup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	raw := observeStore(t, real)
	var count int
	if err := raw.QueryRow("SELECT count(*) FROM buckets WHERE node_id='revived'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("revival replayed successfully deleted history from old pending: rows=%d err=%v", count, err)
	}
	// 复活只恢复接纳新的样本，不能重造删除前的桶。
	s.Samples() <- Sample{"revived", now, map[string]float64{"cpu_percent": 7}}
	if _, err := s.Cleanup(t.Context(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, step := range []int{60, 300, 3600} {
		var total float64
		var samples int64
		if err := raw.QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE node_id='revived' AND step=?", step).Scan(&total, &samples); err != nil || total != 7 || samples != 1 {
			t.Fatalf("revival did not retain only new data: step=%d total=%v samples=%d err=%v", step, total, samples, err)
		}
	}
}

func TestHistoryDeleteRevivalKeepsSamplesAdmittedAtBoundary(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t).(*sqliteStore)
	real.nodes = main
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := real.Write(t.Context(), []Bucket{{Node: "revived", Series: "cpu_percent", T: now.Add(-2 * time.Minute).Unix(), Total: 1, Peak: 1, Samples: 1}}); err != nil {
		t.Fatal(err)
	}
	insertTestTombstone(t, main, "revived", now)
	var service *Service
	boundary := &deletionBoundaryStore{Store: real}
	boundary.afterDelete = func(ctx context.Context) error {
		boundary.afterDelete = nil
		lease, err := main.Acquire(ctx, storage.Writer)
		if err != nil {
			return err
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(lease.Context(), "INSERT INTO nodes(id,name,created_at) VALUES('revived','revived',?)", timekey.Format(now)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if err := service.refreshDeletions(ctx); err != nil {
			return err
		}
		// 与删除批次间的 yield 一样，在同一 run goroutine 接纳复活后的新样本。
		service.add(Sample{"revived", now.Add(-time.Minute), map[string]float64{"cpu_percent": 3}})
		return nil
	}
	service = newService(boundary, main, Options{Now: func() time.Time { return now }})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	service.Samples() <- Sample{"revived", now.Add(-time.Minute), diskValues("/before-delete")}
	if _, err := service.Cleanup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	for _, step := range []int{60, 300, 3600} {
		var total float64
		var samples int64
		if err := observeStore(t, real).QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE node_id='revived' AND step=?", step).Scan(&total, &samples); err != nil || total != 3 || samples != 1 {
			t.Fatalf("deletion consumed new data or restored old pending: step=%d total=%v samples=%d err=%v", step, total, samples, err)
		}
	}
}

func TestHistoryDeleteRevivalDoesNotReuseOldPathQuota(t *testing.T) {
	main := emptyTestMain(t)
	real := openTestSQLiteStore(t).(*sqliteStore)
	real.nodes = main
	now := time.Now().UTC().Truncate(time.Minute)
	var buckets []Bucket
	for i := range maxDiskPaths {
		buckets = append(buckets, Bucket{Node: "revived", Series: "disk_used_percent:/old-" + string(rune('a'+i)), T: now.Add(-2 * time.Minute).Unix(), Total: 2, Peak: 2, Samples: 1})
	}
	if _, err := real.Write(t.Context(), buckets); err != nil {
		t.Fatal(err)
	}
	insertTestTombstone(t, main, "revived", now)
	var service *Service
	boundary := &deletionBoundaryStore{Store: real}
	boundary.beforeYield = func(ctx context.Context) error {
		boundary.beforeYield = nil
		lease, err := main.Acquire(ctx, storage.Writer)
		if err != nil {
			return err
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(lease.Context(), "INSERT INTO nodes(id,name,created_at) VALUES('revived','revived',?)", timekey.Format(now)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if err := service.refreshDeletions(ctx); err != nil {
			return err
		}
		// 真实删除已提交并释放租约，在它返回前从同一 run goroutine 接纳新路径。
		service.add(Sample{"revived", now.Add(-time.Minute), map[string]float64{"disk_used_percent:/new": 1}})
		return nil
	}
	service = newService(boundary, main, Options{Now: func() time.Time { return now }})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	// 从 SQL 恢复满额的 16 个旧路径，形成真实旧缓存与删除前 pending。
	service.Samples() <- Sample{"revived", now.Add(-time.Minute), map[string]float64{"disk_used_percent:/old-a": 2}}
	if _, err := service.Cleanup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	for _, step := range []int{60, 300, 3600} {
		var count int
		var total float64
		var samples int64
		if err := observeStore(t, real).QueryRow("SELECT count(*),coalesce(sum(total),0),coalesce(sum(samples),0) FROM buckets WHERE node_id='revived' AND step=?", step).Scan(&count, &total, &samples); err != nil || count != 1 || total != 1 || samples != 1 {
			t.Fatalf("old path quota rejected revived sample: step=%d rows=%d total=%v samples=%d rejected=%d err=%v", step, count, total, samples, service.SeriesRejected(), err)
		}
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(service.paths["revived"]) != 1 {
		t.Fatalf("new path cache was cleared or old quota survived: %v", service.paths["revived"])
	}
}
