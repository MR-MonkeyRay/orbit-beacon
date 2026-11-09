package history

import (
	"context"
	"math"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"strings"
	"testing"
	"time"
)

func TestMySQLHistoryBatchBoundsAndOrder(t *testing.T) {
	buckets := make([]Bucket, 600)
	for i := range buckets {
		buckets[i] = Bucket{Node: "00000000-0000-4000-8000-000000000001", Series: "inode_used_percent:" + strings.Repeat("x", 1024), T: int64(i) * 60, Total: float64(i), Peak: float64(i), Samples: 1}
	}
	next, total := 0, 0
	for next < len(buckets)*3 {
		query, args, end, err := mysqlHistoryBatch(buckets, next)
		if err != nil {
			t.Fatal(err)
		}
		if end <= next || end-next > 256 || len(args) != (end-next)*7 || len(query)+historyArgsBytes(args) > 1<<20 {
			t.Fatalf("unbounded batch rows=%d args=%d bytes=%d err=%v", end-next, len(args), len(query)+historyArgsBytes(args), err)
		}
		for i := next; i < end; i++ {
			b := buckets[i/3]
			step := []int64{60, 300, 3600}[i%3]
			offset := (i - next) * 7
			if args[offset] != b.Node || args[offset+1] != b.Series || args[offset+2] != step || args[offset+3] != time.Unix(b.T, 0).Truncate(time.Duration(step)*time.Second).Unix() || args[offset+4] != b.Total {
				t.Fatalf("batch changed input order at %d", i)
			}
		}
		total += end - next
		next = end
	}
	if total != 1800 {
		t.Fatalf("rows=%d", total)
	}
	oversized := []Bucket{{Node: "node", Series: strings.Repeat("x", 1<<20), Samples: 1}}
	if _, _, _, err := mysqlHistoryBatch(oversized, 0); err == nil {
		t.Fatal("oversized row accepted")
	}
}

func TestSQLHistoryPreservesFloatAccumulationOrder(t *testing.T) {
	store := openTestSQLiteStore(t)
	ctx := context.Background()
	for _, total := range []float64{1e16, -1e16, 1} {
		if _, err := store.Write(ctx, []Bucket{{Node: "one", Series: "disk_used_percent:" + strings.Repeat("x", 1024), T: 3600, Total: total, Peak: math.SmallestNonzeroFloat64, Samples: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []int{60, 300, 3600} {
		points, err := store.Query(ctx, "one", "disk_used_percent:"+strings.Repeat("x", 1024), step, 3600, 3600)
		if err != nil || len(points) != 1 || points[0].Avg != 1.0/3 || points[0].Max != math.SmallestNonzeroFloat64 {
			t.Fatalf("step=%d points=%v err=%v", step, points, err)
		}
	}
}

func TestMySQLHistoryStore(t *testing.T) {
	fixture := storagetest.Start(t)
	database := fixture.Open(t, storage.HistoryDomain)
	testStoreContract(t, func(t *testing.T) Store { return &sqliteStore{db: database} }, nil)
	store := &sqliteStore{db: database}
	ctx := context.Background()
	node := "00000000-0000-4000-8000-000000000001"
	for _, path := range []string{strings.Repeat("x", 1024), strings.Repeat("路", 341) + "x"} {
		series := "inode_used_percent:" + path
		buckets := make([]Bucket, 600)
		for i := range buckets {
			buckets[i] = Bucket{Node: node, Series: series, T: 3600, Total: 0.125, Peak: 0.25, Samples: 1}
		}
		if n, err := store.Write(ctx, buckets); err != nil || n != 1800 {
			t.Fatalf("rows=%d err=%v", n, err)
		}
		for _, step := range []int{60, 300, 3600} {
			points, err := store.Query(ctx, node, series, step, 3600, 3600)
			if err != nil || len(points) != 1 || points[0].Avg != 0.125 || points[0].Max != 0.25 {
				t.Fatalf("step=%d points=%v err=%v", step, points, err)
			}
		}
	}
	// 大小写与尾空格属于不同主键；seek 必须完整返回所有序列。
	for _, series := range []string{"Cpu", "cpu", "cpu "} {
		if _, err := store.Write(ctx, []Bucket{{Node: node, Series: series, T: 3600, Total: 1, Peak: 1, Samples: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	series, err := store.Series(ctx, node)
	if err != nil || len(series) != 5 {
		t.Fatalf("series=%v err=%v", series, err)
	}
	ordered := []Bucket{{Node: node, Series: "order", T: 3600, Total: 1e16, Peak: math.SmallestNonzeroFloat64, Samples: 1}, {Node: node, Series: "order", T: 3600, Total: -1e16, Peak: math.SmallestNonzeroFloat64, Samples: 1}, {Node: node, Series: "order", T: 3600, Total: 1, Peak: math.SmallestNonzeroFloat64, Samples: 1}}
	if _, err = store.Write(ctx, ordered); err != nil {
		t.Fatal(err)
	}
	points, err := store.Query(ctx, node, "order", 3600, 3600, 3600)
	if err != nil || len(points) != 1 || points[0].Avg != 1.0/3 || points[0].Max != math.SmallestNonzeroFloat64 {
		t.Fatalf("float points=%v err=%v", points, err)
	}
	before := fixture.Raw(t, storage.HistoryDomain)
	var total float64
	if err = before.QueryRow(`SELECT total FROM buckets WHERE node_id=? AND metric='order' AND step=3600`, node).Scan(&total); err != nil || total != 1 {
		t.Fatalf("total=%v err=%v", total, err)
	}
	if _, err = store.Write(ctx, []Bucket{{Node: "", Series: "", T: 3600, Total: 1, Peak: 1, Samples: 1}}); err != nil {
		t.Fatal(err)
	}
	emptySeries, err := store.Series(ctx, "")
	if err != nil || len(emptySeries) != 1 || emptySeries[0] != "" {
		t.Fatalf("empty seek=%v err=%v", emptySeries, err)
	}
	foundEmpty := false
	if err = store.NodeIDs(ctx, func(id string) {
		if id == "" {
			foundEmpty = true
		}
	}); err != nil || !foundEmpty {
		t.Fatalf("empty node seek: found=%t err=%v", foundEmpty, err)
	}
	// 同一事务在第二批失败也必须撤回已累加的第一批。
	bad := make([]Bucket, 300)
	for i := range bad {
		bad[i] = Bucket{Node: node, Series: "rollback", T: int64(i) * 60, Total: 1, Peak: 1, Samples: 1}
	}
	bad[len(bad)-1].Samples = 0
	if _, err = store.Write(ctx, bad); err == nil {
		t.Fatal("invalid batch succeeded")
	}
	var count int
	if err = before.QueryRow(`SELECT count(*) FROM buckets WHERE metric='rollback'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
	if _, err = store.DeleteNode(ctx, node, func() {}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteExpired(ctx, time.Now().Add(200*24*time.Hour), func() {}); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLHistoryResolvedReceiptRetriesOnlyRolledBackBatch(t *testing.T) {
	fixture := storagetest.Start(t)
	database := fixture.Open(t, storage.HistoryDomain)
	store := &sqliteStore{db: database}
	for _, committed := range []bool{false, true} {
		node := "rolledback"
		if committed {
			node = "committed"
		}
		buckets := []Bucket{{Node: node, Series: "cpu_percent", T: 3600, Total: 20, Peak: 20, Samples: 1}}
		lease, err := database.Acquire(t.Context(), storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
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
		// 模拟协调器已解析原 UUID、Service 尚未消费结果的边界。
		store.unresolved, store.inflight = tx, append([]Bucket(nil), buckets...)
		if _, err := store.Write(t.Context(), buckets); err != nil {
			t.Fatal(err)
		}
		if store.unresolved != nil || len(store.inflight) != 0 {
			t.Fatal("已解析收据未释放")
		}
		buckets[0].Total, buckets[0].Peak = 30, 30
		if _, err := store.Write(t.Context(), buckets); err != nil {
			t.Fatal(err)
		}
		for _, step := range []int{60, 300, 3600} {
			var total float64
			var samples int64
			if err := fixture.Raw(t, storage.HistoryDomain).QueryRow("SELECT total,samples FROM buckets WHERE node_id=? AND step=?", node, step).Scan(&total, &samples); err != nil || total != 50 || samples != 2 {
				t.Fatalf("step=%d total=%v samples=%d err=%v", step, total, samples, err)
			}
		}
	}
}
