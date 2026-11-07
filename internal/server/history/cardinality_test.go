package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func diskValues(path string) map[string]float64 {
	return map[string]float64{"disk_used_percent:" + path: 25, "inode_used_percent:" + path: 50, "disk_used_percent": 30, "inode_used_percent": 60}
}

func TestPathCardinalityKnownAndAggregateContinue(t *testing.T) {
	// add and path inventory are owned by the single run goroutine in production.
	s := &Service{pending: make(map[bucketKey]map[string]aggregate)}
	now := time.Unix(60, 0)
	for i := range 17 {
		s.add(Sample{"one", now, diskValues(fmt.Sprintf("/disk:%d", i))})
	}
	if len(s.paths["one"]) != 16 || s.SeriesRejected() != 2 {
		t.Fatalf("paths=%v rejected=%d", s.paths, s.SeriesRejected())
	}
	s.add(Sample{"one", now, diskValues("/disk:0")})
	s.add(Sample{"two", now, diskValues("/disk:16")})
	if s.SeriesRejected() != 2 || len(s.paths["two"]) != 1 {
		t.Fatalf("paths=%v rejected=%d", s.paths, s.SeriesRejected())
	}
	values := s.pending[bucketKey{"one", 60}]
	if _, exists := values["disk_used_percent:/disk:16"]; exists {
		t.Fatal("17th path admitted")
	}
	if len(values) != 34 || values["disk_used_percent:/disk:0"].samples != 2 || values["inode_used_percent:/disk:0"].samples != 2 || values["disk_used_percent"].samples != 18 || values["inode_used_percent"].samples != 18 {
		t.Fatalf("values=%v", values)
	}
}

func TestPathCardinalityReloadsFromDatabase(t *testing.T) {
	s, dir := testService(t, nil)
	now := time.Now().UTC()
	for i := range 16 {
		s.samples <- Sample{"one", now, diskValues(fmt.Sprintf("/disk:%d", i))}
	}
	stopped(t, s)
	restarted, err := Open(context.Background(), dir, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.samples <- Sample{"one", now, diskValues("/new")}
	restarted.samples <- Sample{"one", now, diskValues("/disk:0")}
	stopped(t, restarted)
	if restarted.SeriesRejected() != 2 || len(restarted.paths["one"]) != 16 {
		t.Fatalf("paths=%v rejected=%d", restarted.paths, restarted.SeriesRejected())
	}
	result, err := restarted.Query(context.Background(), "one", "disk_used_percent", "1h", now, "/new")
	if err != nil || len(result.Points) != 0 {
		t.Fatalf("new=%+v err=%v", result, err)
	}
	result, err = restarted.Query(context.Background(), "one", "inode_used_percent", "1h", now, "/disk:0")
	if err != nil || len(result.Points) != 1 || result.Points[0].Avg != 50 {
		t.Fatalf("known=%+v err=%v", result, err)
	}
	// Expiration keeps the in-memory reservation; a fresh Service uses only DB contents.
	if _, err := sqlitePools(t, restarted).db.Exec(`DELETE FROM buckets WHERE node_id=? AND metric IN (?,?)`, "one", "disk_used_percent:/disk:15", "inode_used_percent:/disk:15"); err != nil {
		t.Fatal(err)
	}
	if restarted.admitPath("one", "/replacement") {
		t.Fatal("expiration immediately released a path")
	}
	fresh, err := Open(context.Background(), dir, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	fresh.samples <- Sample{"one", now, diskValues("/replacement")}
	stopped(t, fresh)
	if fresh.SeriesRejected() != 0 || len(fresh.paths["one"]) != 16 {
		t.Fatalf("paths=%v rejected=%d", fresh.paths, fresh.SeriesRejected())
	}
}

func TestConcurrentProducersCannotExceedPathLimit(t *testing.T) {
	s, _ := testService(t, nil)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() { defer wg.Done(); s.samples <- Sample{"one", now, diskValues(fmt.Sprintf("/disk:%d", i))} }()
	}
	wg.Wait()
	stopped(t, s)
	if len(s.paths["one"]) != 16 || s.SeriesRejected() != 96 {
		t.Fatalf("paths=%v rejected=%d", s.paths, s.SeriesRejected())
	}
	var count int
	if err := sqlitePools(t, s).db.QueryRow(`SELECT count(DISTINCT metric) FROM buckets WHERE node_id='one' AND metric LIKE '%:%'`).Scan(&count); err != nil || count != 32 {
		t.Fatalf("series=%d err=%v", count, err)
	}
}

func TestSeriesQueryWalksThePrimaryKey(t *testing.T) {
	s, _ := testService(t, nil)
	if _, err := sqlitePools(t, s).db.Exec(`INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES
		('one','a',60,1,1,1,1),('one','a',60,2,1,1,1),('one','b',60,1,1,1,1),('two','c',60,1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	rows, err := sqlitePools(t, s).db.Query(`EXPLAIN QUERY PLAN `+historySeriesQuery, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	skips := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "buckets") && !strings.HasPrefix(detail, "SEARCH") {
			t.Fatalf("plan: %s", detail)
		}
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Fatalf("plan: %s", detail)
		}
		skips = skips || strings.Contains(detail, "metric>?")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// DISTINCT also searches the primary key without a temp tree, but it visits every bucket.
	if !skips {
		t.Fatal("plan does not seek to the next metric")
	}
	found, err := sqlitePools(t, s).db.Query(historySeriesQuery, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer found.Close()
	var metrics []string
	for found.Next() {
		var metric string
		if err := found.Scan(&metric); err != nil {
			t.Fatal(err)
		}
		metrics = append(metrics, metric)
	}
	if fmt.Sprint(metrics) != "[a b]" {
		t.Fatalf("metrics=%v", metrics)
	}
}

func TestCleanupForgetsPathsOfDeletedNodes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	main, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	s, _ := testService(t, main)
	stopped(t, s)
	if !s.admitPath("deleted", "/a") {
		t.Fatal("path rejected")
	}
	if _, err := s.cleanup(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, kept := s.paths["deleted"]; kept {
		t.Fatal("deleted node keeps its path reservation")
	}
}
