package history

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

// A trigger supplies samples once per delete batch, exercising the same
// non-blocking admission as ingest without relying on wall-clock scheduling.
var cleanupProducer atomic.Pointer[func()]

func init() {
	sqlite.MustRegisterScalarFunction("history_test_cleanup_produce", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if produce := cleanupProducer.Load(); produce != nil {
			(*produce)()
		}
		return int64(0), nil
	})
}

func TestLargeCleanupKeepsAcceptingSamples(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(fmt.Sprint("orphan=", orphan), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			main, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer main.Close()
			live, err := nodes.NewService(main, nodes.Options{}).CreatePending(context.Background(), nodes.CreateRequest{Name: "live"})
			if err != nil {
				t.Fatal(err)
			}
			s, _ := testService(t, main)
			now := time.Now().UTC().Truncate(time.Minute)
			id := live.ID
			if orphan {
				id = "deleted"
			}
			const batches = 12
			const rows = batches * 1000
			_, err = s.db.Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?) INSERT INTO buckets SELECT ?, 'cpu_percent', 60, ?-n*60, 10, 10, 1 FROM seq`, rows, id, now.Add(-Retention1m).Unix())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`CREATE TRIGGER cleanup_producer BEFORE DELETE ON buckets WHEN OLD.t % 60000 = 0 BEGIN SELECT history_test_cleanup_produce(); END`); err != nil {
				t.Fatal(err)
			}
			var offered, dropped int
			produce := func() {
				for range 512 {
					offered++
					select {
					case s.samples <- Sample{live.ID, now.Add(-time.Minute), map[string]float64{"cpu_percent": 25}}:
					default:
						dropped++
					}
				}
			}
			cleanupProducer.Store(&produce)
			defer cleanupProducer.Store(nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			removed, err := s.Cleanup(ctx, now)
			if err != nil || removed != rows {
				t.Fatalf("removed=%d err=%v", removed, err)
			}
			stopped(t, s)
			if offered <= SampleBufferSize || dropped != 0 || s.DroppedBuckets() != 0 {
				t.Fatalf("offered=%d dropped=%d buckets_dropped=%d", offered, dropped, s.DroppedBuckets())
			}
			var accepted int
			if err := s.db.QueryRow(`SELECT samples FROM buckets WHERE node_id=? AND step=60`, live.ID).Scan(&accepted); err != nil || accepted != offered {
				t.Fatalf("accepted=%d offered=%d err=%v", accepted, offered, err)
			}
		})
	}
}

func TestOrphanDiscoveryUsesNodeSeeks(t *testing.T) {
	s, _ := testService(t, nil)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + historyNodesQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan, "SCAN buckets") || !strings.Contains(plan, "SEARCH buckets USING PRIMARY KEY (node_id>?)") {
		t.Fatalf("orphan discovery must seek past each node's buckets:\n%s", plan)
	}
}

func TestAggregationRejectsOverflowingInputs(t *testing.T) {
	s, _ := testService(t, nil)
	now := time.Now().UTC().Truncate(time.Minute)
	for _, value := range []float64{1e308, 1e308, math.Inf(1), math.NaN(), 1e6, 0} {
		s.samples <- Sample{"node", now, map[string]float64{"load1": value}}
	}
	stopped(t, s)
	for _, requestedRange := range []string{"1h", "24h", "30d"} {
		result, err := s.Query(context.Background(), "node", "load1", requestedRange, now)
		if err != nil || len(result.Points) != 1 || result.Points[0].Avg != 5e5 || result.Points[0].Max != 1e6 {
			t.Fatalf("range=%s result=%+v err=%v", requestedRange, result, err)
		}
		if _, err := json.Marshal(result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQuerySkipsNonFiniteStoredPoints(t *testing.T) {
	s, _ := testService(t, nil)
	stopped(t, s)
	now := time.Now().UTC().Truncate(time.Minute)
	for i, values := range [][2]float64{{math.Inf(1), 1}, {1, math.Inf(1)}, {math.Inf(-1), 1}, {1, math.Inf(-1)}, {10, 10}} {
		if _, err := s.db.Exec(`INSERT INTO buckets VALUES('node','load1',60,?,?,?,1)`, now.Add(time.Duration(-i)*time.Minute).Unix(), values[0], values[1]); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Query(context.Background(), "node", "load1", "1h", now)
	if err != nil || len(result.Points) != 1 || result.Points[0].Avg != 10 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatal(err)
	}
}

func testService(t *testing.T, main *sql.DB) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, main, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dir
}

func stopped(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAggregationWeightedRollupsShutdownAndGaps(t *testing.T) {
	s, _ := testService(t, nil)
	now := time.Now().UTC().Truncate(time.Hour)
	for _, v := range []struct {
		at    time.Time
		value float64
	}{
		{now.Add(10 * time.Second), 10}, {now.Add(20 * time.Second), 30},
		{now.Add(time.Minute), 80}, {now.Add(3 * time.Minute), 0},
	} {
		s.samples <- Sample{NodeID: "one", ReceivedAt: v.at, Values: map[string]float64{"cpu_percent": v.value}}
	}
	// A second node must never affect the first node's math.
	s.samples <- Sample{NodeID: "two", ReceivedAt: now, Values: map[string]float64{"cpu_percent": 99}}
	stopped(t, s)
	for _, test := range []struct {
		rangeName          string
		count              int
		firstAvg, firstMax float64
	}{
		{"1h", 3, 20, 30}, {"6h", 3, 20, 30}, {"24h", 1, 30, 80}, {"7d", 1, 30, 80}, {"30d", 1, 30, 80},
	} {
		response, err := s.Query(context.Background(), "one", "cpu_percent", test.rangeName, now.Add(4*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Points) != test.count || response.Points[0].Avg != test.firstAvg || response.Points[0].Max != test.firstMax || !response.Points[0].T.Equal(now) {
			t.Fatalf("range=%s points=%+v", test.rangeName, response.Points)
		}
	}
}

func TestFlushCompletedOnlyAndRestartMergesPartialMinute(t *testing.T) {
	s, dir := testService(t, nil)
	// Stop the writer before exercising the internal aggregation functions.
	stopped(t, s)
	now := time.Date(2026, 10, 27, 12, 20, 0, 0, time.UTC)
	s.add(Sample{"node", now.Add(-time.Minute), map[string]float64{"cpu_percent": 20}})
	s.add(Sample{"node", now.Add(time.Second), map[string]float64{"cpu_percent": 40}})
	if err := s.flush(context.Background(), now.Add(5*time.Second), false); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 1 {
		t.Fatalf("pending=%d", len(s.pending))
	}
	if err := s.flush(context.Background(), now, true); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(context.Background(), dir, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.samples <- Sample{"node", now.Add(10 * time.Second), map[string]float64{"cpu_percent": 80}}
	stopped(t, restarted)
	response, err := restarted.Query(context.Background(), "node", "cpu_percent", "1h", now.Add(20*time.Second))
	if err != nil || len(response.Points) != 2 || response.Points[1].Avg != 60 || response.Points[1].Max != 80 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	rollup, err := restarted.Query(context.Background(), "node", "cpu_percent", "30d", now.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, point := range rollup.Points {
		total += point.Avg
	}
	// Within the same hour, all three values are weighted together.
	if len(rollup.Points) != 1 || total != float64(140)/3 {
		t.Fatalf("rollup=%+v", rollup)
	}
}

func TestRetentionAndDeletedNodePurge(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	main, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	ns := nodes.NewService(main, nodes.Options{})
	live, err := ns.CreatePending(context.Background(), nodes.CreateRequest{Name: "live"})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := ns.CreatePending(context.Background(), nodes.CreateRequest{Name: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := testService(t, main)
	stopped(t, s)
	now := time.Now().UTC().Truncate(time.Hour)
	for _, id := range []string{live.ID, deleted.ID} {
		for _, tier := range []struct {
			step int
			keep time.Duration
		}{{60, Retention1m}, {300, Retention5m}, {3600, Retention1h}} {
			for _, timestamp := range []int64{now.Add(-tier.keep).Unix() - 1, now.Add(-tier.keep).Unix(), now.Unix()} {
				if _, err := s.db.Exec(`INSERT INTO buckets VALUES(?, 'cpu_percent', ?, ?, 10, 10, 1)`, id, tier.step, timestamp); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	s.add(Sample{deleted.ID, now, map[string]float64{"cpu_percent": 90}})
	if err := ns.Delete(context.Background(), deleted.ID, true); err != nil {
		t.Fatal(err)
	}
	removed, err := s.cleanup(context.Background(), now)
	if err != nil || removed != 12 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM buckets WHERE node_id=?`, deleted.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan rows=%d err=%v", count, err)
	}
	if len(s.pending) != 0 {
		t.Fatal("orphan pending bucket survived")
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM buckets WHERE node_id=?`, live.ID).Scan(&count); err != nil || count != 6 {
		t.Fatalf("retained rows=%d err=%v", count, err)
	}
}

func TestIndependentWriterLockAndCleanupDrainsQueue(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	main, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	ns := nodes.NewService(main, nodes.Options{})
	live, err := ns.CreatePending(context.Background(), nodes.CreateRequest{Name: "node"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := testService(t, main)
	tx, err := main.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Truncate(time.Minute)
	s.samples <- Sample{live.ID, now.Add(-time.Minute), map[string]float64{"cpu_percent": 33}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Cleanup(ctx, now); err != nil {
		t.Fatalf("main writer blocked history: %v", err)
	}
	response, err := s.Query(ctx, live.ID, "cpu_percent", "1h", now)
	if err != nil || len(response.Points) != 1 {
		t.Fatalf("queued sample not flushed: %+v %v", response, err)
	}
}

func TestSchemaTooNewAndPermissions(t *testing.T) {
	s, dir := testService(t, nil)
	stopped(t, s)
	if _, err := s.db.Exec(`INSERT INTO schema_migrations VALUES(?, 'future')`, CurrentSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	newer, err := Open(context.Background(), dir, nil, Options{})
	if newer != nil || !errors.Is(err, storage.ErrSchemaTooNew) {
		t.Fatalf("newer=%v err=%v", newer, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(filepath.Join(dir, "metrics.db") + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("unsafe %s mode %v", suffix, info.Mode())
		}
	}
}

func TestSelectRangeAndEmpty(t *testing.T) {
	for name, step := range map[string]int{"1h": 60, "6h": 60, "24h": 300, "7d": 300, "30d": 3600} {
		selection, err := SelectRange(name)
		if err != nil || selection.StepSeconds != step {
			t.Fatalf("%s=%+v %v", name, selection, err)
		}
	}
	for _, name := range []string{"", "1H", "48h", "0h", " 1h", "1h "} {
		if _, err := SelectRange(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	response, err := Empty("net_rx_bytes_per_second", "1h")
	if err != nil || response.Points == nil || len(response.Points) != 0 || response.Unit != "bytes_per_second" {
		t.Fatalf("empty=%+v %v", response, err)
	}
	if _, err := Empty("unknown", "1h"); err == nil {
		t.Fatal("accepted unknown metric")
	}
}

func TestConcurrentSamplesAndShutdown(t *testing.T) {
	s, _ := testService(t, nil)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				s.samples <- Sample{"node", now, map[string]float64{"cpu_percent": 25}}
			}
		}()
	}
	wg.Wait()
	stopped(t, s)
	stopped(t, s) // idempotent
	var count int
	if err := s.db.QueryRow(`SELECT sum(samples) FROM buckets WHERE step=60`).Scan(&count); err != nil || count != 800 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestFailedFlushKeepsPendingAndCapacityIsBounded(t *testing.T) {
	s, _ := testService(t, nil)
	stopped(t, s)
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"node", now.Add(-time.Minute), map[string]float64{"cpu_percent": 25}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.flush(ctx, now, false); !errors.Is(err, context.Canceled) || len(s.pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(s.pending), err)
	}
	if err := s.flush(context.Background(), now, false); err != nil {
		t.Fatal(err)
	}
	for i := range maxPendingBuckets + 1 {
		s.add(Sample{"node", now.Add(time.Duration(i) * time.Minute), map[string]float64{"cpu_percent": 25}})
	}
	if len(s.pending) != maxPendingBuckets || s.DroppedBuckets() != 1 {
		t.Fatalf("pending=%d dropped=%d", len(s.pending), s.DroppedBuckets())
	}
}

func TestShutdownDeadlineInterruptsBlockedWriter(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxOpenConns(1)
	held, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	s.samples <- Sample{"node", time.Now(), map[string]float64{"cpu_percent": 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = s.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("shutdown took %v err=%v", time.Since(start), err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("writer survived canceled shutdown")
	}
	// Deadline errors are expected; database closure still completes.
	_ = s.Close()
}

func TestOpenRejectsUnsafeHistoryFileAndSidecars(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "metrics.db") + suffix
			if err := os.WriteFile(path, nil, 0644); err != nil {
				t.Fatal(err)
			}
			if service, err := Open(context.Background(), dir, nil, Options{}); err == nil {
				service.Close()
				t.Fatal("opened unsafe history component")
			}
		})
	}
}

func TestNetworkRateAggregationOmitsMissingSamples(t *testing.T) {
	s, _ := testService(t, nil)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, values := range []map[string]float64{
		{"net_rx_bytes_per_second": 100, "net_tx_bytes_per_second": 20},
		{"cpu_percent": 50}, // a failed/missing network measurement contributes no zero
		{"net_rx_bytes_per_second": 300, "net_tx_bytes_per_second": 0},
	} {
		s.samples <- Sample{"node", now, values}
	}
	stopped(t, s)
	for _, test := range []struct {
		metric    string
		avg, peak float64
	}{{"net_rx_bytes_per_second", 200, 300}, {"net_tx_bytes_per_second", 10, 20}} {
		result, err := s.Query(context.Background(), "node", test.metric, "1h", now.Add(time.Minute))
		if err != nil || len(result.Points) != 1 || result.Points[0].Avg != test.avg || result.Points[0].Max != test.peak {
			t.Fatalf("%s=%+v err=%v", test.metric, result, err)
		}
	}
}

func TestRetentionPreservesIntersectingBoundaryBuckets(t *testing.T) {
	s, _ := testService(t, nil)
	stopped(t, s)
	now := time.Date(2026, 10, 27, 12, 34, 30, 0, time.UTC)
	for _, tier := range []struct {
		step int
		keep time.Duration
	}{{60, Retention1m}, {300, Retention5m}, {3600, Retention1h}} {
		cutoff := now.Add(-tier.keep).Truncate(time.Duration(tier.step) * time.Second)
		for _, timestamp := range []int64{cutoff.Unix() - int64(tier.step), cutoff.Unix()} {
			if _, err := s.db.Exec(`INSERT INTO buckets VALUES('node','cpu_percent',?,?,10,10,1)`, tier.step, timestamp); err != nil {
				t.Fatal(err)
			}
		}
	}
	removed, err := s.cleanup(context.Background(), now)
	if err != nil || removed != 3 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	for _, step := range []int{60, 300, 3600} {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM buckets WHERE step=?`, step).Scan(&count); err != nil || count != 1 {
			t.Fatalf("step=%d count=%d err=%v", step, count, err)
		}
	}
	// The same minute boundary is preserved in pending memory before flush.
	cutoff := now.Add(-Retention1m).Truncate(time.Minute)
	s.add(Sample{"pending", cutoff, map[string]float64{"cpu_percent": 10}})
	s.add(Sample{"expired", cutoff.Add(-time.Minute), map[string]float64{"cpu_percent": 10}})
	if _, err := s.cleanup(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM buckets WHERE node_id='pending'`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("pending count=%d err=%v", count, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM buckets WHERE node_id='expired'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired count=%d err=%v", count, err)
	}
}
