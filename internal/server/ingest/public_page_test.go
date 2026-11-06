package ingest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"orbitbeacon/internal/server/storage"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

// connector 统计实际的驱动查询次数，并能在页面查询返回行之前将其阻断。
// 投影不得对该数据库发起任何按节点查询。
type publicPageConnector struct {
	queries atomic.Int64
	gate    <-chan struct{}
	now     time.Time
	entered chan struct{}
}

func (c *publicPageConnector) Connect(context.Context) (driver.Conn, error) {
	return &publicPageConn{connector: c}, nil
}
func (c *publicPageConnector) Driver() driver.Driver { return publicPageDriver{} }

type publicPageDriver struct{}

func (publicPageDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type publicPageConn struct{ connector *publicPageConnector }

func (*publicPageConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*publicPageConn) Close() error              { return nil }
func (*publicPageConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (c *publicPageConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.connector.queries.Add(1)
	if c.connector.entered != nil {
		c.connector.entered <- struct{}{}
	}
	if len(args) != 4 {
		return nil, fmt.Errorf("unexpected per-node query: %s", query)
	}
	if c.connector.gate != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.connector.gate:
		}
	}
	remaining := int(args[0].Value.(int64))
	return &publicPageRows{remaining: remaining, now: c.connector.now}, nil
}

type publicPageRows struct {
	remaining, index int
	now              time.Time
}

func (*publicPageRows) Columns() []string {
	return []string{"id", "public_settings", "last_seen", "token_hash", "credential_epoch", "run_generation", "report_session_id", "availability"}
}
func (*publicPageRows) Close() error { return nil }
func (r *publicPageRows) Next(dest []driver.Value) error {
	if r.remaining == 0 {
		return io.EOF
	}
	r.remaining--
	r.index++
	values := []driver.Value{fmt.Sprintf("public-%d", r.index), `{"enabled":true,"name":"Public"}`, timekey.Format(r.now), make([]byte, 32), int64(0), "1", "session", "[]"}
	copy(dest, values)
	return nil
}

func TestPublicPageQueryCountDoesNotScaleWithPageSize(t *testing.T) {
	for _, limit := range []int{1, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
			connector := &publicPageConnector{now: now}
			db := sql.OpenDB(connector)
			t.Cleanup(func() { db.Close() })
			s := NewService(db, Options{})
			ns := nodes.NewService(db, nodes.Options{})
			out, err := s.PublicPage(context.Background(), ns, limit, 0, 10*time.Second, func() time.Time { return now })
			if err != nil || len(out.Nodes) != limit || !out.HasMore || out.NextOffset == nil || *out.NextOffset != limit {
				t.Fatalf("page: %+v err=%v", out, err)
			}
			if got := connector.queries.Load(); got != 1 {
				t.Fatalf("%d nodes required %d queries", limit, got)
			}
			for _, node := range out.Nodes {
				if len(node.Availability.Days) != 30 || node.Availability.Percent != nil || node.Availability.StartDay != "2026-09-28" {
					t.Fatalf("empty availability %+v", node.Availability)
				}
				for _, percent := range node.Availability.Days {
					if percent != nil {
						t.Fatal("unobserved day must be null")
					}
				}
			}
			if len(s.states) != 0 {
				t.Fatalf("public page created %d ingest states", len(s.states))
			}
		})
	}
}

func TestPublicPageCoalescesMissesAndAllowsWaiterCancellation(t *testing.T) {
	const requests = 20
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	gate := make(chan struct{})
	var release sync.Once
	connector := &publicPageConnector{now: now, gate: gate}
	db := sql.OpenDB(connector)
	defer db.Close()
	defer release.Do(func() { close(gate) })
	s := NewService(db, Options{})
	ns := nodes.NewService(db, nodes.Options{})
	entered := make(chan struct{}, 2*requests+2)
	clock := func() time.Time { entered <- struct{}{}; return now }
	results := make(chan error, requests)
	for range requests {
		go func() {
			page, err := s.PublicPage(ctx, ns, 100, 0, 10*time.Second, clock)
			if err == nil && len(page.Nodes) != 100 {
				err = errors.New("incomplete page")
			}
			results <- err
		}()
	}
	for range requests {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("requests did not reach cache")
		}
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	waitResult := make(chan error, 1)
	go func() { _, err := s.PublicPage(waitCtx, ns, 100, 0, 10*time.Second, clock); waitResult <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("cancelled waiter did not reach cache")
	}
	cancelWait()
	if err := <-waitResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation: %v", err)
	}
	release.Do(func() { close(gate) })
	for range requests {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := connector.queries.Load(); got != 1 {
		t.Fatalf("coalesced page executed %d queries", got)
	}
}

func TestProjectPublicRecordChecksEveryFenceWithoutCreatingState(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	s := NewService(nil, Options{})
	record := nodes.PublicRecord{ID: "public", Settings: nodes.PublicSettings{Name: "Public"}, LastSeen: now, Generation: 1, Session: "session"}
	out, _ := s.ProjectPublicRecord(record, now)
	if out.Status != "online" || out.CPUPercent != nil || len(s.states) != 0 {
		t.Fatalf("cold projection: %+v states=%d", out, len(s.states))
	}
	state := s.acquireState(record.ID)
	state.generation, state.session = record.Generation, record.Session
	state.public = publicMetrics{receivedAt: now, cpu: publicMetric{valid: true, value: 25, intervalMS: 10000}}
	s.releaseState(state)
	out, _ = s.ProjectPublicRecord(record, now)
	if out.CPUPercent == nil || *out.CPUPercent != 25 {
		t.Fatalf("matching fence: %+v", out)
	}
	for _, fence := range []string{"credential", "credential_epoch", "generation", "session", "received_at"} {
		t.Run(fence, func(t *testing.T) {
			changed := record
			switch fence {
			case "credential":
				changed.Credential[0]++
			case "credential_epoch":
				changed.CredentialEpoch++
			case "generation":
				changed.Generation++
			case "session":
				changed.Session += "-new"
			case "received_at":
				changed.LastSeen = changed.LastSeen.Add(-time.Nanosecond)
			}
			out, _ := s.ProjectPublicRecord(changed, now)
			if out.Status != "online" || out.CPUPercent != nil {
				t.Fatalf("mismatched %s: %+v", fence, out)
			}
		})
	}
}

func TestPublicPageQueriesBeforeTakingStateLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	connector := &publicPageConnector{now: now, entered: make(chan struct{}, 1)}
	db := sql.OpenDB(connector)
	defer db.Close()
	s := NewService(db, Options{})
	state := s.acquireState("public-1")
	s.releaseState(state)
	state.mu.Lock()
	var unlock sync.Once
	defer unlock.Do(state.mu.Unlock)
	done := make(chan error, 1)
	go func() {
		_, err := s.PublicPage(ctx, nodes.NewService(db, nodes.Options{}), 1, 0, time.Second, func() time.Time { return now })
		done <- err
	}()
	select {
	case <-connector.entered:
	case <-ctx.Done():
		t.Fatal("database query waited for ingest state mutex")
	}
	unlock.Do(state.mu.Unlock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPublicPageCacheUsesEffectiveWebIntervalAndStaysBounded(t *testing.T) {
	for _, seconds := range []int{2, 10, 30, 60} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
			connector := &publicPageConnector{now: now}
			db := sql.OpenDB(connector)
			defer db.Close()
			s := NewService(db, Options{})
			ns := nodes.NewService(db, nodes.Options{})
			ttl := time.Duration(seconds) * time.Second
			clock := func() time.Time { return now }
			first, err := s.PublicPage(context.Background(), ns, 1, 0, ttl, clock)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(ttl - time.Nanosecond)
			cached, err := s.PublicPage(context.Background(), ns, 1, 0, ttl, clock)
			if err != nil || !cached.GeneratedAt.Equal(first.GeneratedAt) || connector.queries.Load() != 1 {
				t.Fatalf("cache expired before %v: err=%v queries=%d", ttl, err, connector.queries.Load())
			}
			now = now.Add(time.Nanosecond)
			expired, err := s.PublicPage(context.Background(), ns, 1, 0, ttl, clock)
			if err != nil || !expired.GeneratedAt.Equal(now) || connector.queries.Load() != 2 {
				t.Fatalf("cache did not expire at %v: err=%v queries=%d", ttl, err, connector.queries.Load())
			}
			for offset := 1; offset <= 140; offset++ {
				if _, err := s.PublicPage(context.Background(), ns, 1, offset, ttl, clock); err != nil {
					t.Fatal(err)
				}
			}
			if len(s.publicPages.entries) > 128 {
				t.Fatalf("unbounded pages: %d", len(s.publicPages.entries))
			}
		})
	}
}

// Wrap the real SQLite driver: counting queries must not replace SQL execution.
type availabilityCountingConnector struct {
	driver  driver.Driver
	dsn     string
	queries atomic.Int64
}

func (c *availabilityCountingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return availabilityCountingConn{Conn: conn, queries: &c.queries}, nil
}
func (c *availabilityCountingConnector) Driver() driver.Driver { return c.driver }

type availabilityCountingConn struct {
	driver.Conn
	queries *atomic.Int64
}

func (c availabilityCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func TestPublicAvailabilityRealSQLiteBatchAndCache(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "public.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("private-node-%d", i)
		if _, err := db.Exec(`INSERT INTO nodes(id,name,status,token_hash,first_enrolled_at,created_at,public_settings,run_generation,report_session_id,last_seen)
 VALUES(?,?,'unknown',?,?,?,? ,?,?,?)`, id, "private-name", []byte(strings.Repeat(fmt.Sprint(i), 32)), timekey.Format(now), timekey.Format(now), `{"enabled":true,"name":"Public"}`, fmt.Sprint(i), "session", timekey.Format(now)); err != nil {
			t.Fatal(err)
		}
		// 每个节点的百分比不同，串节点时断言会失败。
		if _, err := db.Exec(`INSERT INTO node_availability_daily VALUES(?,'2026-11-04',?,?)`, id, 86400-(i+1)*8640, (i+1)*8640); err != nil {
			t.Fatal(err)
		}
	}
	connector := &availabilityCountingConnector{driver: db.Driver(), dsn: path}
	reader := sql.OpenDB(connector)
	defer reader.Close()
	ns := nodes.NewService(db, nodes.Options{})
	ns.SetPublicReader(reader)
	s := NewService(db, Options{})
	for range 2 {
		page, err := s.PublicPage(ctx, ns, 100, 0, time.Minute, func() time.Time { return now })
		if err != nil || len(page.Nodes) != 3 {
			t.Fatalf("page %v %v", page, err)
		}
		for i, n := range page.Nodes {
			want := float64(90 - i*10)
			if n.Availability.StartDay != "2026-10-07" || len(n.Availability.Days) != 30 || *n.Availability.Percent != want || *n.Availability.Days[28] != want || n.Availability.Days[29] != nil {
				t.Fatalf("projection %+v", n.Availability)
			}
		}
		if page.Nodes[0].Status != "unknown" {
			t.Fatal("identity not ready should be unknown")
		}
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-node", "private-name", "online_seconds", "offline_seconds", "node_id"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("leaked %s", raw)
			}
		}
	}
	if queries := connector.queries.Load(); queries != 1 {
		t.Fatalf("batch + cache queries %d", queries)
	}
}
