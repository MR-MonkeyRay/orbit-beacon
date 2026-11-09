package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func openTestSQLiteStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openSQLiteStore(context.Background(), filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	registerStoreObserver(t, store, filepath.Join(dir, "metrics.db"))
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestSQLiteStoreContract(t *testing.T) {
	testStoreContract(t, openTestSQLiteStore, nil)
}

// 通用契约只检查可观察的序列行为，不依赖存储行数或 SQLite 保留边界。
func testStoreContract(t *testing.T, open func(t *testing.T) Store, settle func(*testing.T, Store)) {
	flush := func(t *testing.T, store Store) {
		if settle != nil {
			settle(t, store)
		}
	}
	t.Run("merge_ranges_order_and_isolation", func(t *testing.T) {
		store := open(t)
		ctx := context.Background()
		prefix := fmt.Sprintf("orbitbeacon-f2-%d-", time.Now().UnixNano())
		one, two := prefix+"one", prefix+"two"
		var existing []string
		if err := store.NodeIDs(ctx, func(id string) { existing = append(existing, id) }); err != nil {
			t.Fatal(err)
		}
		base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
		for _, buckets := range [][]Bucket{
			{
				{Node: one, Series: "cpu_percent", T: base.Add(2 * time.Hour).Unix(), Total: 80, Peak: 80, Samples: 1},
				{Node: one, Series: "cpu_percent", T: base.Unix(), Total: 40, Peak: 30, Samples: 2},
				{Node: one, Series: "cpu_percent", T: base.Add(time.Hour).Unix(), Total: 50, Peak: 50, Samples: 1},
				{Node: two, Series: "cpu_percent", T: base.Unix(), Total: 99, Peak: 99, Samples: 1},
				{Node: one, Series: "memory_used_percent", T: base.Unix(), Total: 70, Peak: 70, Samples: 1},
				{Node: one, Series: "disk_used_percent:/data", T: base.Unix(), Total: 60, Peak: 60, Samples: 1},
				{Node: one, Series: "disk_used_percent", T: base.Unix(), Total: 20, Peak: 20, Samples: 1},
			},
			{{Node: one, Series: "cpu_percent", T: base.Unix(), Total: 80, Peak: 80, Samples: 1}},
		} {
			if _, err := store.Write(ctx, buckets); err != nil {
				t.Fatal(err)
			}
		}
		flush(t, store)
		for _, step := range []int{60, 300, 3600} {
			t.Run(fmt.Sprint(step), func(t *testing.T) {
				points, err := store.Query(ctx, one, "cpu_percent", step, base.Unix(), base.Add(2*time.Hour).Unix())
				want := []Point{{T: base, Avg: 40, Max: 80}, {T: base.Add(time.Hour), Avg: 50, Max: 50}, {T: base.Add(2 * time.Hour), Avg: 80, Max: 80}}
				if err != nil || !reflect.DeepEqual(points, want) {
					t.Fatalf("points=%+v want=%+v err=%v", points, want, err)
				}
				points, err = store.Query(ctx, one, "cpu_percent", step, base.Unix()+int64(step), base.Add(2*time.Hour).Unix()-int64(step))
				if err != nil || len(points) != 1 || !points[0].T.Equal(base.Add(time.Hour)) {
					t.Fatalf("aligned inner bounds: points=%+v err=%v", points, err)
				}
				for _, tc := range []struct {
					node, series string
					value        float64
				}{{two, "cpu_percent", 99}, {one, "memory_used_percent", 70}, {one, "disk_used_percent:/data", 60}, {one, "disk_used_percent", 20}} {
					points, err := store.Query(ctx, tc.node, tc.series, step, base.Unix(), base.Unix())
					if err != nil || len(points) != 1 || points[0].Avg != tc.value || points[0].Max != tc.value {
						t.Fatalf("node=%s series=%s points=%+v err=%v", tc.node, tc.series, points, err)
					}
				}
			})
		}
		series, err := store.Series(ctx, one)
		slices.Sort(series)
		if err != nil || !reflect.DeepEqual(series, []string{"cpu_percent", "disk_used_percent", "disk_used_percent:/data", "memory_used_percent"}) {
			t.Fatalf("series=%v err=%v", series, err)
		}
		var ids []string
		if err := store.NodeIDs(ctx, func(id string) { ids = append(ids, id) }); err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		wantIDs := append(append([]string(nil), existing...), one, two)
		slices.Sort(wantIDs)
		if !reflect.DeepEqual(ids, wantIDs) {
			t.Fatalf("node IDs=%v", ids)
		}
		if _, err := store.DeleteNode(ctx, one, func() {}); err != nil {
			t.Fatal(err)
		}
		flush(t, store)
		ids = nil
		if err := store.NodeIDs(ctx, func(id string) { ids = append(ids, id) }); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(ids, one) || !slices.Contains(ids, two) {
			t.Fatalf("node IDs after delete=%v", ids)
		}
		for _, step := range []int{60, 300, 3600} {
			for _, series := range []string{"cpu_percent", "memory_used_percent", "disk_used_percent:/data", "disk_used_percent"} {
				points, err := store.Query(ctx, one, series, step, base.Unix(), base.Add(2*time.Hour).Unix())
				if err != nil || len(points) != 0 {
					t.Fatalf("deleted series=%s points=%+v err=%v", series, points, err)
				}
			}
			points, err := store.Query(ctx, two, "cpu_percent", step, base.Unix(), base.Unix())
			if err != nil || len(points) != 1 || points[0].Avg != 99 {
				t.Fatalf("other node points=%+v err=%v", points, err)
			}
		}
	})
	t.Run("canceled_write_is_atomic", func(t *testing.T) {
		store := open(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		base := time.Now().UTC().Truncate(time.Hour)
		node := fmt.Sprintf("orbitbeacon-f2-canceled-%d", time.Now().UnixNano())
		if _, err := store.Write(ctx, []Bucket{{Node: node, Series: "cpu_percent", T: base.Unix(), Total: 10, Peak: 10, Samples: 1}}); err == nil {
			t.Fatal("canceled write succeeded")
		}
		for _, step := range []int{60, 300, 3600} {
			points, err := store.Query(context.Background(), node, "cpu_percent", step, base.Unix(), base.Unix())
			if err != nil || len(points) != 0 {
				t.Fatalf("canceled write stored points=%+v err=%v", points, err)
			}
		}
	})
}

func TestSQLiteStoreRetentionBoundaries(t *testing.T) {
	store := openTestSQLiteStore(t).(*sqliteStore)
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 34, 30, 0, time.UTC)
	for _, tier := range []struct {
		step int
		keep time.Duration
	}{{60, Retention1m}, {300, Retention5m}, {3600, Retention1h}} {
		cutoff := now.Add(-tier.keep).Truncate(time.Duration(tier.step) * time.Second).Unix()
		for _, timestamp := range []int64{cutoff - int64(tier.step), cutoff, cutoff + int64(tier.step)} {
			if _, err := observeStore(t, store).ExecContext(ctx, `INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES('one','cpu_percent',?,?,10,10,1)`, tier.step, timestamp); err != nil {
				t.Fatal(err)
			}
		}
	}
	yields := 0
	removed, err := store.DeleteExpired(ctx, now, func() { yields++ })
	if err != nil || removed != 3 || yields != 3 {
		t.Fatalf("removed=%d yields=%d err=%v", removed, yields, err)
	}
	for _, tier := range []struct {
		step int
		keep time.Duration
	}{{60, Retention1m}, {300, Retention5m}, {3600, Retention1h}} {
		cutoff := now.Add(-tier.keep).Truncate(time.Duration(tier.step) * time.Second)
		points, err := store.Query(ctx, "one", "cpu_percent", tier.step, cutoff.Unix()-int64(tier.step), now.Unix())
		if err != nil || len(points) != 2 || !points[0].T.Equal(cutoff) || !points[1].T.Equal(cutoff.Add(time.Duration(tier.step)*time.Second)) {
			t.Fatalf("step=%d points=%+v err=%v", tier.step, points, err)
		}
	}
}

func TestSQLiteStoreWriteRowsAndRollback(t *testing.T) {
	store := openTestSQLiteStore(t)
	buckets := []Bucket{{Node: "one", Series: "cpu_percent", T: 60, Total: 10, Peak: 10, Samples: 1}, {Node: "two", Series: "load1", T: 60, Total: 20, Peak: 20, Samples: 1}}
	if rows, err := store.Write(context.Background(), buckets); err != nil || rows != 6 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, err := store.Write(ctx, buckets); err == nil || rows != 6 {
		t.Fatalf("failed rows=%d err=%v", rows, err)
	}
	// 第二个桶违反 schema 约束，前面已执行的 upsert 也必须回滚。
	buckets[1].Samples = 0
	if rows, err := store.Write(context.Background(), buckets); err == nil || rows != 6 {
		t.Fatalf("invalid rows=%d err=%v", rows, err)
	}
	var total float64
	if err := observeStore(t, store.(*sqliteStore)).QueryRow(`SELECT total FROM buckets WHERE node_id='one' AND step=60`).Scan(&total); err != nil || total != 10 {
		t.Fatalf("rollback total=%v err=%v", total, err)
	}
}

func TestSQLiteStoreReportsPools(t *testing.T) {
	if _, _, ok := openTestSQLiteStore(t).DatabaseStats(); !ok {
		t.Fatal("SQLite store hid its connection pools")
	}
}

// 查询固定桶起点，非对齐边界不改变聚合网格。
func TestSQLiteStoreUnalignedRange(t *testing.T) {
	store := openTestSQLiteStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	for _, node := range []string{"one", "two"} {
		for i := 0; i < 3; i++ {
			if _, err := store.Write(ctx, []Bucket{{Node: node, Series: "cpu_percent", T: base.Add(time.Duration(i) * time.Hour).Unix(), Total: 10, Peak: 10, Samples: 1}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, step := range []int{60, 300, 3600} {
		points, err := store.Query(ctx, "one", "cpu_percent", step, base.Unix()+1, base.Add(2*time.Hour).Unix()-1)
		if err != nil || len(points) != 1 || !points[0].T.Equal(base.Add(time.Hour)) {
			t.Fatalf("exclusive outside bounds: points=%+v err=%v", points, err)
		}
	}
}
