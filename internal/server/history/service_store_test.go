package history

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"maps"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"
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

func stoppedStoreService(t *testing.T, store Store, nodes *sql.DB, opts Options) *Service {
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

// 空节点快照使用内存驱动，隔离历史清理的失败语义与文件权限检查。
type emptyNodesConnector struct{}
type emptyNodesDriver struct{}
type emptyNodesConn struct{}
type emptyNodesRows struct{}

func (emptyNodesConnector) Connect(context.Context) (driver.Conn, error) {
	return emptyNodesConn{}, nil
}
func (emptyNodesConnector) Driver() driver.Driver          { return emptyNodesDriver{} }
func (emptyNodesDriver) Open(string) (driver.Conn, error)  { return emptyNodesConn{}, nil }
func (emptyNodesConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (emptyNodesConn) Close() error                        { return nil }
func (emptyNodesConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (emptyNodesConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != "SELECT id FROM nodes" {
		return nil, errors.New("unexpected nodes query")
	}
	return emptyNodesRows{}, nil
}
func (emptyNodesRows) Columns() []string         { return []string{"id"} }
func (emptyNodesRows) Close() error              { return nil }
func (emptyNodesRows) Next([]driver.Value) error { return io.EOF }

func TestServiceStoreFailedDeleteKeepsPathsAndPending(t *testing.T) {
	failure := errors.New("delete failed")
	store := &fakeStore{deleteErr: failure, ids: []string{"orphan"}}
	nodes := sql.OpenDB(emptyNodesConnector{})
	defer nodes.Close()
	s := stoppedStoreService(t, store, nodes, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"orphan", now.Add(-time.Minute), diskValues("/data")})
	key := bucketKey{"orphan", now.Add(-time.Minute).Unix()}
	pending := maps.Clone(s.pending[key])
	removed, err := s.cleanup(context.Background(), now)
	if removed != 0 || !errors.Is(err, failure) || len(s.pending) != 1 || !reflect.DeepEqual(s.pending[key], pending) || len(s.paths["orphan"]) != 1 {
		t.Fatalf("removed=%d pending=%v paths=%v err=%v", removed, s.pending, s.paths, err)
	}
	if !reflect.DeepEqual(store.deleted, []string{"orphan"}) || store.expired || len(store.writes) != 0 {
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
	s := stoppedStoreService(t, store, nil, Options{})
	if s.admitPath("node", "/new") || len(s.paths) != 0 {
		t.Fatal("failed inventory admitted path")
	}
	store.seriesErr = nil
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

func TestServiceStoreCleanupFreezesCandidatesBeforeDraining(t *testing.T) {
	store := &fakeStore{}
	nodes := sql.OpenDB(emptyNodesConnector{})
	defer nodes.Close()
	s := stoppedStoreService(t, store, nodes, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	s.add(Sample{"pending", now.Add(-time.Minute), map[string]float64{"cpu_percent": 10}})
	s.paths["paths"] = map[string]struct{}{"/data": {}}
	store.nodeIDs = func(visit func(string)) error {
		// 扫描历史节点时排空的新样本不应追加到已冻结的内存候选中。
		s.samples <- Sample{"late", now.Add(-time.Minute), diskValues("/late")}
		visit("stored")
		return nil
	}
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
