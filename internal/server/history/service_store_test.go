package history

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

type fakeStore struct {
	nodeIDs                        func(func(string)) error
	writeErr, deleteErr, seriesErr error
	queryErr                       error
	writes                         [][]Bucket
	ids, series                    []string
	points                         []Point
	query                          func(string, string, int, int64, int64)
	deleted                        []string
	seriesCalls                    int
	expired                        bool
	closed                         bool
}

func (f *fakeStore) Write(_ context.Context, buckets []Bucket) (int, error) {
	f.writes = append(f.writes, append([]Bucket(nil), buckets...))
	return 3 * len(buckets), f.writeErr
}
func (f *fakeStore) Query(_ context.Context, node, series string, step int, start, end int64) ([]Point, error) {
	if f.query != nil {
		f.query(node, series, step, start, end)
	}
	return f.points, f.queryErr
}
func (f *fakeStore) Series(context.Context, string) ([]string, error) {
	f.seriesCalls++
	return f.series, f.seriesErr
}
func (f *fakeStore) NodeIDs(_ context.Context, visit func(string)) error {
	if f.nodeIDs != nil {
		return f.nodeIDs(visit)
	}
	for _, id := range f.ids {
		visit(id)
	}
	return nil
}
func (f *fakeStore) DeleteNode(_ context.Context, node string, yield func()) (int64, error) {
	f.deleted = append(f.deleted, node)
	yield()
	return 0, f.deleteErr
}
func (f *fakeStore) DeleteExpired(_ context.Context, _ time.Time, yield func()) (int64, error) {
	f.expired = true
	yield()
	return 0, nil
}
func (f *fakeStore) DatabaseStats() (sql.DBStats, sql.DBStats, bool) {
	return sql.DBStats{}, sql.DBStats{}, false
}
func (f *fakeStore) Close() error { f.closed = true; return nil }

func stoppedStoreService(t *testing.T, store Store, nodes *storage.Database, opts Options) *Service {
	t.Helper()
	s := newService(store, nodes, opts)
	stopped(t, s)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestServiceStoreFailedWriteRetainsPending(t *testing.T) {
	failure := errors.New("write failed")
	store := &fakeStore{writeErr: failure}
	var observed []int
	s := stoppedStoreService(t, store, nil, Options{ObserveFlush: func(_ time.Duration, rows int) { observed = append(observed, rows) }})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"node", now.Add(-time.Minute), map[string]float64{"cpu_percent": 20}})
	s.add(Sample{"node", now, map[string]float64{"cpu_percent": 99}})
	if err := s.flush(context.Background(), now, false); !errors.Is(err, failure) || len(s.pending) != 2 {
		t.Fatalf("pending=%v err=%v", s.pending, err)
	}
	want := []Bucket{{Node: "node", Series: "cpu_percent", T: now.Add(-time.Minute).Unix(), Total: 20, Peak: 20, Samples: 1}}
	if len(store.writes) != 1 || !reflect.DeepEqual(store.writes[0], want) {
		t.Fatalf("writes=%+v want=%+v", store.writes, want)
	}
	store.writeErr = nil
	if err := s.flush(context.Background(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 1 || len(store.writes) != 2 || !reflect.DeepEqual(store.writes[1], want) || !reflect.DeepEqual(observed, []int{3, 3}) {
		t.Fatalf("pending=%v writes=%+v observations=%v", s.pending, store.writes, observed)
	}
	if _, ok := s.pending[bucketKey{"node", now.Unix()}]; !ok {
		t.Fatal("current minute was released")
	}
}

func TestUnknownHistoryCommitKeepsImmutableInflight(t *testing.T) {
	store := &fakeStore{writeErr: storage.ErrCommitUnknown}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Minute)
	s.add(Sample{"node", at, map[string]float64{"cpu_percent": 20}})
	if err := s.flush(context.Background(), now, false); !errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatal(err)
	}
	if len(s.inflight) != 1 || len(s.pending) != 0 {
		t.Fatalf("inflight=%v pending=%v", s.inflight, s.pending)
	}
	s.add(Sample{"node", at, map[string]float64{"cpu_percent": 30}})
	if s.inflight[0].Total != 20 {
		t.Fatal("new sample changed uncertain batch")
	}
	store.writeErr = nil
	if err := s.flush(context.Background(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 3 || !reflect.DeepEqual(store.writes[0], store.writes[1]) || store.writes[2][0].Total != 30 || len(s.inflight) != 0 || len(s.pending) != 0 {
		t.Fatalf("writes=%v pending=%v inflight=%v", store.writes, s.pending, s.inflight)
	}
}

func emptyTestMain(t *testing.T) *storage.Database {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenMain(t.Context(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestServiceStoreFailedDeleteKeepsPathsAndPending(t *testing.T) {
	failure := errors.New("delete failed")
	store := &fakeStore{deleteErr: failure, ids: []string{"orphan"}}
	nodes := emptyTestMain(t)
	s := stoppedStoreService(t, store, nodes, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"orphan", now.Add(-time.Minute), diskValues("/data")})
	key := bucketKey{"orphan", now.Add(-time.Minute).Unix()}
	pending := maps.Clone(s.pending[key])
	insertTestTombstone(t, nodes, "orphan", now)
	removed, err := s.cleanup(context.Background(), now)
	if removed != 0 || !errors.Is(err, failure) || len(s.pending) != 1 || !reflect.DeepEqual(s.pending[key], pending) || len(s.paths["orphan"]) != 1 {
		t.Fatalf("removed=%d pending=%v paths=%v err=%v", removed, s.pending, s.paths, err)
	}
	if !reflect.DeepEqual(store.deleted, []string{"orphan"}) || !store.expired || len(store.writes) != 0 {
		t.Fatalf("deletes=%v expired=%v writes=%v", store.deleted, store.expired, store.writes)
	}
	store.deleteErr = nil
	if _, err := s.cleanup(context.Background(), now); err != nil || len(s.pending) != 0 || len(s.paths) != 0 || !store.expired {
		t.Fatalf("retry pending=%v paths=%v expired=%v err=%v", s.pending, s.paths, store.expired, err)
	}
}

func TestServiceStoreQueryShapingAndStats(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 34, 30, 0, time.UTC)
	good := Point{T: now.Add(-time.Minute), Avg: 25, Max: 30}
	store := &fakeStore{points: []Point{{Avg: math.NaN()}, {Avg: math.Inf(1)}, {Max: math.Inf(-1)}, {Max: math.NaN()}, good}}
	store.query = func(node, series string, step int, start, end int64) {
		if node != "node" || series != "disk_used_percent:/data" || step != 300 || start != now.Add(-24*time.Hour).Truncate(5*time.Minute).Unix() || end != now.Unix() {
			t.Fatalf("query=%s %s %d %d %d", node, series, step, start, end)
		}
	}
	s := stoppedStoreService(t, store, nil, Options{})
	response, err := s.Query(context.Background(), "node", "disk_used_percent", "24h", now, "/data")
	if err != nil || response.Unit != "percent" || response.StepSeconds != 300 || !reflect.DeepEqual(response.Points, []Point{good}) {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if _, _, ok := s.DatabaseStats(); ok {
		t.Fatal("store without pools reported database stats")
	}
}

func TestServiceStorePathInventoryRetries(t *testing.T) {
	store := &fakeStore{seriesErr: errors.New("inventory failed"), series: []string{"disk_used_percent:/data", "inode_used_percent:/data", "cpu_percent"}}
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	s := stoppedStoreService(t, store, nil, Options{Now: func() time.Time { return now }})
	if s.admitPath("node", "/new") || len(s.paths) != 0 {
		t.Fatal("failed inventory admitted path")
	}
	store.seriesErr = nil
	if s.admitPath("other", "/new") || len(s.paths) != 0 {
		t.Fatal("retried inventory during backoff")
	}
	if store.seriesCalls != 1 {
		t.Fatalf("inventory calls during backoff=%d", store.seriesCalls)
	}
	now = now.Add(seriesRetryDelay)
	if !s.admitPath("node", "/new") || len(s.paths["node"]) != 2 {
		t.Fatalf("paths=%v", s.paths)
	}
}

func TestServiceStoreQueryErrors(t *testing.T) {
	failure := errors.New("query failed")
	store := &fakeStore{queryErr: failure}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC()
	response, err := s.Query(context.Background(), "node", "cpu_percent", "1h", now, "")
	if !errors.Is(err, failure) || !reflect.DeepEqual(response, Response{}) {
		t.Fatalf("query failure response=%+v err=%v", response, err)
	}
}

func TestServiceStoreCleanupUsesOnlyTombstonesAndDrains(t *testing.T) {
	store := &fakeStore{}
	nodes := emptyTestMain(t)
	s := stoppedStoreService(t, store, nodes, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"pending", now.Add(-time.Minute), map[string]float64{"cpu_percent": 10}})
	s.paths["paths"] = map[string]struct{}{"/data": {}}
	for _, id := range []string{"pending", "paths", "stored"} {
		insertTestTombstone(t, nodes, id, now)
	}
	s.samples <- Sample{"late", now.Add(-time.Minute), diskValues("/late")}
	if _, err := s.cleanup(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	slices.Sort(store.deleted)
	if !reflect.DeepEqual(store.deleted, []string{"paths", "pending", "stored"}) || len(s.paths) != 1 || len(s.paths["late"]) != 1 || len(s.pending) != 0 {
		t.Fatalf("deleted=%v paths=%v pending=%v", store.deleted, s.paths, s.pending)
	}
	if len(store.writes) != 1 || len(store.writes[0]) != 4 {
		t.Fatalf("writes=%+v", store.writes)
	}
	for _, bucket := range store.writes[0] {
		if bucket.Node != "late" {
			t.Fatalf("orphan survived: %+v", bucket)
		}
	}
}
