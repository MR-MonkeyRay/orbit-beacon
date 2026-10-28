package server

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

type publicStreamStore struct {
	mu        sync.Mutex
	visible   bool
	queries   int
	queryErr  error
	queryWait <-chan struct{}
}

type publicStreamConnector struct{ store *publicStreamStore }

func (c publicStreamConnector) Connect(context.Context) (driver.Conn, error) {
	return publicStreamConn(c), nil
}

func (c publicStreamConnector) Driver() driver.Driver { return publicStreamDriver{} }

type publicStreamDriver struct{}

func (publicStreamDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("public stream test driver requires a connector")
}

type publicStreamConn struct{ store *publicStreamStore }

func (publicStreamConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (publicStreamConn) Close() error                        { return nil }
func (publicStreamConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c publicStreamConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.Lock()
	c.store.queries++
	visible := c.store.visible
	err := c.store.queryErr
	wait := c.store.queryWait
	c.store.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	rows := &publicStreamRows{}
	if visible {
		rows.values = [][]driver.Value{{
			"00000000-0000-4000-8000-000000000001",
			`{"enabled":true,"name":"Public node","region":"Earth","provider":"Test"}`,
			nil,
			bytes.Repeat([]byte{1}, 32),
			"0",
			"",
		}}
	}
	return rows, nil
}

type publicStreamRows struct {
	values [][]driver.Value
	index  int
}

func (*publicStreamRows) Columns() []string {
	return []string{"id", "public_settings", "last_seen", "token_hash", "run_generation", "report_session_id"}
}
func (*publicStreamRows) Close() error { return nil }
func (r *publicStreamRows) Next(dest []driver.Value) error {
	if r.index == len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

type streamRecorder struct {
	mu          sync.Mutex
	header      http.Header
	status      int
	body        bytes.Buffer
	flushed     chan string
	deadlines   int
	cleared     int
	deadlineErr error
	deadline    time.Time
	flushErr    error
	writeErr    error
	shortWrite  bool
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header), flushed: make(chan string, 16)}
}

func (w *streamRecorder) Header() http.Header { return w.header }
func (w *streamRecorder) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}
func (w *streamRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.shortWrite && len(p) > 0 {
		_, _ = w.body.Write(p[:len(p)-1])
		return len(p) - 1, nil
	}
	return w.body.Write(p)
}
func (w *streamRecorder) FlushError() error {
	w.mu.Lock()
	snapshot := w.body.String()
	err := w.flushErr
	w.mu.Unlock()
	if err == nil {
		w.flushed <- snapshot
	}
	return err
}
func (w *streamRecorder) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	// Count armed deadlines; zero clears the previous one between frames.
	if deadline.IsZero() {
		w.cleared++
	} else {
		w.deadlines++
		w.deadline = deadline
	}
	err := w.deadlineErr
	w.mu.Unlock()
	return err
}
func (w *streamRecorder) Deadline() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deadline
}
func (w *streamRecorder) Body() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}
func (w *streamRecorder) DeadlineCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deadlines
}
func (w *streamRecorder) ClearedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cleared
}

type streamFixture struct {
	h     *handler
	store *publicStreamStore
	db    *sql.DB
}

func newStreamFixture(t *testing.T, visible bool) *streamFixture {
	t.Helper()
	store := &publicStreamStore{visible: visible}
	db := sql.OpenDB(publicStreamConnector{store: store})
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	ns := nodes.NewService(db, nodes.Options{Now: func() time.Time { return now }})
	h := &handler{
		nodes:           ns,
		ingest:          ingest.NewService(db, ingest.Options{Now: func() time.Time { return now }}),
		metrics:         newHTTPMetrics("test"),
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:             func() time.Time { return now },
		streamInterval:  20 * time.Millisecond,
		streamHeartbeat: time.Second,
		streamLifetime:  time.Second,
	}
	t.Cleanup(func() { _ = db.Close() })
	return &streamFixture{h: h, store: store, db: db}
}

func startPublicStream(t *testing.T, h *handler, recorder *streamRecorder) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "https://beacon.test/api/public/v1/stream", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(recorder, request)
		close(done)
	}()
	return cancel, done
}

func nextFlush(t *testing.T, recorder *streamRecorder) string {
	t.Helper()
	select {
	case snapshot := <-recorder.flushed:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("stream did not flush")
		return ""
	}
}

func waitStreamDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not end")
	}
}

func TestPublicStreamFirstFrameImmediateAndExact(t *testing.T) {
	f := newStreamFixture(t, false)
	recorder := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, recorder)
	started := time.Now()
	frame := nextFlush(t, recorder)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("first frame took %v", elapsed)
	}
	want := "event: nodes\ndata: {\"nodes\":[],\"has_more\":false,\"next_offset\":null,\"generated_at\":\"2026-03-04T05:06:07Z\"}\n\n"
	if frame != want {
		t.Fatalf("first frame = %q, want %q", frame, want)
	}
	for name, want := range map[string]string{
		"Content-Type":              "text/event-stream; charset=utf-8",
		"Cache-Control":             "no-store",
		"X-Accel-Buffering":         "no",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "same-origin",
		"Strict-Transport-Security": "max-age=31536000",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if recorder.DeadlineCount() != 1 {
		t.Fatalf("write deadlines = %d, want 1", recorder.DeadlineCount())
	}
	if remaining := time.Until(recorder.Deadline()); remaining < 9*time.Second || remaining > 10*time.Second {
		t.Fatalf("write deadline remaining = %v, want approximately 10s", remaining)
	}
	// An armed deadline left idle would reset HTTP/2 streams before the heartbeat.
	for wait := time.Now().Add(time.Second); recorder.ClearedCount() != 1; {
		if time.Now().After(wait) {
			t.Fatalf("cleared deadlines = %d, want 1", recorder.ClearedCount())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	waitStreamDone(t, done)
	if strings.Contains(recorder.Body(), "event: close") {
		t.Fatal("client cancellation emitted a close frame")
	}
}

func TestPublicStreamHeartbeat(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.streamHeartbeat = 35 * time.Millisecond
	f.h.streamInterval = time.Second
	recorder := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, recorder)
	defer func() { cancel(); waitStreamDone(t, done) }()
	first := nextFlush(t, recorder)
	second := nextFlush(t, recorder)
	if second != first+first {
		t.Fatalf("heartbeat did not repeat nodes frame: %q", second)
	}
}

func TestPublicStreamCoalescesChangeNotifications(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.streamInterval = 60 * time.Millisecond
	recorder := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, recorder)
	defer func() { cancel(); waitStreamDone(t, done) }()
	first := nextFlush(t, recorder)
	for range 5 {
		f.h.nodes.InvalidatePublic()
	}
	select {
	case <-recorder.flushed:
		t.Fatal("change frame bypassed coalescing interval")
	case <-time.After(25 * time.Millisecond):
	}
	second := nextFlush(t, recorder)
	if strings.Count(second, "event: nodes\n") != 2 || !strings.HasPrefix(second, first) {
		t.Fatalf("coalesced stream = %q", second)
	}
	select {
	case extra := <-recorder.flushed:
		t.Fatalf("notifications produced an extra frame: %q", extra)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestPublicStreamDiscardsPageInvalidatedDuringQuery(t *testing.T) {
	f := newStreamFixture(t, true)
	blocked := make(chan struct{})
	f.store.mu.Lock()
	f.store.queryWait = blocked
	f.store.mu.Unlock()
	recorder := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, recorder)
	defer func() { cancel(); waitStreamDone(t, done) }()
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		f.store.mu.Lock()
		started := f.store.queries > 0
		f.store.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PublicPage query did not start")
		}
	}
	// The in-flight query already read the node as published.
	f.store.mu.Lock()
	f.store.visible = false
	f.store.queryWait = nil
	f.store.mu.Unlock()
	f.h.nodes.InvalidatePublic()
	close(blocked)
	if first := nextFlush(t, recorder); strings.Contains(first, "Public node") || !strings.Contains(first, `data: {"nodes":[]`) {
		t.Fatalf("frame emitted a page invalidated during its query: %q", first)
	}
}

func TestPublicStreamUnpublishInvalidatesCachedPage(t *testing.T) {
	f := newStreamFixture(t, true)
	recorder := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, recorder)
	defer func() { cancel(); waitStreamDone(t, done) }()
	first := nextFlush(t, recorder)
	if !strings.Contains(first, `"name":"Public node"`) {
		t.Fatalf("published node absent from first frame: %q", first)
	}
	f.store.mu.Lock()
	f.store.visible = false
	f.store.mu.Unlock()
	f.h.nodes.InvalidatePublic()
	second := nextFlush(t, recorder)
	frames := strings.Split(second, "\n\nevent: nodes\n")
	if len(frames) != 2 || strings.Contains(frames[1], "Public node") || !strings.Contains(frames[1], `data: {"nodes":[]`) {
		t.Fatalf("unpublish not visible in next cached PublicPage frame: %q", second)
	}
	f.store.mu.Lock()
	queries := f.store.queries
	f.store.mu.Unlock()
	if queries != 2 {
		t.Fatalf("public page queries = %d, want one initial query and one version-invalidated query", queries)
	}
}

func TestPublicStreamCapacityLimits(t *testing.T) {
	t.Run("per IP", func(t *testing.T) {
		f := newStreamFixture(t, false)
		for range 4 {
			if _, ok := f.h.streams.acquire("192.0.2.1"); !ok {
				t.Fatal("failed to seed per-IP streams")
			}
		}
		defer func() {
			for range 4 {
				f.h.streams.release("192.0.2.1")
			}
		}()
		response := httptest.NewRecorder()
		f.h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil))
		assertStreamBusy(t, response)
	})
	t.Run("global", func(t *testing.T) {
		f := newStreamFixture(t, false)
		for i := range 200 {
			ip := "client-" + string(rune(i))
			if _, ok := f.h.streams.acquire(ip); !ok {
				t.Fatalf("failed to seed global stream %d", i)
			}
			defer f.h.streams.release(ip)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil)
		request.RemoteAddr = "198.51.100.2:1234"
		response := httptest.NewRecorder()
		f.h.ServeHTTP(response, request)
		assertStreamBusy(t, response)
	})
}

func TestPublicStreamPerIPUsesCanonicalTrustedClientAddress(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.trustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for range 4 {
		if _, ok := f.h.streams.acquire("2001:db8::1"); !ok {
			t.Fatal("failed to seed canonical client IP")
		}
	}
	defer func() {
		for range 4 {
			f.h.streams.release("2001:db8::1")
		}
	}()
	request := httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil)
	request.RemoteAddr = "10.1.2.3:54321"
	request.Header.Set("X-Forwarded-For", "2001:0db8:0:0:0:0:0:1")
	response := httptest.NewRecorder()
	f.h.ServeHTTP(response, request)
	assertStreamBusy(t, response)

	// Remote source ports never create distinct per-IP buckets.
	f.h.trustedProxies = nil
	for range 4 {
		if _, ok := f.h.streams.acquire("192.0.2.1"); !ok {
			t.Fatal("failed to seed remote client IP")
		}
	}
	defer func() {
		for range 4 {
			f.h.streams.release("192.0.2.1")
		}
	}()
	request = httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil)
	request.RemoteAddr = "192.0.2.1:65535"
	response = httptest.NewRecorder()
	f.h.ServeHTTP(response, request)
	assertStreamBusy(t, response)
}

func assertStreamBusy(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
		t.Fatalf("busy response = %d Retry-After=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body)
	}
	var result apiError
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Code != "stream_busy" {
		t.Fatalf("busy body = %s, err=%v", response.Body, err)
	}
}

func TestPublicStreamLifetimeAndShutdownClose(t *testing.T) {
	t.Run("lifetime", func(t *testing.T) {
		f := newStreamFixture(t, false)
		f.h.streamLifetime = 35 * time.Millisecond
		recorder := newStreamRecorder()
		_, done := startPublicStream(t, f.h, recorder)
		_ = nextFlush(t, recorder)
		closed := nextFlush(t, recorder)
		waitStreamDone(t, done)
		if !strings.HasSuffix(closed, "event: close\ndata: {}\n\n") {
			t.Fatalf("lifetime close = %q", closed)
		}
		if recorder.DeadlineCount() != 2 {
			t.Fatalf("frame deadlines = %d, want nodes and close", recorder.DeadlineCount())
		}
		if remaining := time.Until(recorder.Deadline()); remaining < 9*time.Second || remaining > 10*time.Second {
			t.Fatalf("close deadline remaining = %v, want approximately 10s", remaining)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		f := newStreamFixture(t, false)
		recorder := newStreamRecorder()
		_, done := startPublicStream(t, f.h, recorder)
		_ = nextFlush(t, recorder)
		f.h.ClosePublicStreams()
		closed := nextFlush(t, recorder)
		waitStreamDone(t, done)
		if !strings.HasSuffix(closed, "event: close\ndata: {}\n\n") {
			t.Fatalf("shutdown close = %q", closed)
		}
		response := httptest.NewRecorder()
		f.h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil))
		assertStreamBusy(t, response)
	})
}

func TestPublicStreamWriteFailuresEndWithoutClose(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*streamRecorder)
	}{
		{"deadline", func(w *streamRecorder) { w.deadlineErr = errors.New("deadline failed") }},
		{"flush", func(w *streamRecorder) { w.flushErr = errors.New("flush failed") }},
		{"write", func(w *streamRecorder) { w.writeErr = errors.New("write failed") }},
		{"short write", func(w *streamRecorder) { w.shortWrite = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newStreamFixture(t, false)
			recorder := newStreamRecorder()
			test.configure(recorder)
			_, done := startPublicStream(t, f.h, recorder)
			waitStreamDone(t, done)
			if strings.Contains(recorder.Body(), "event: close") {
				t.Fatalf("write failure emitted close: %q", recorder.Body())
			}
		})
	}
}

func TestPublicStreamShutdownCancelsBlockedPublicPage(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(*handler)
	}{
		{"shutdown", func(h *handler) { h.ClosePublicStreams() }},
		{"lifetime", func(h *handler) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newStreamFixture(t, false)
			if test.name == "lifetime" {
				f.h.streamLifetime = 35 * time.Millisecond
			}
			blocked := make(chan struct{})
			f.store.mu.Lock()
			f.store.queryWait = blocked
			f.store.mu.Unlock()
			recorder := newStreamRecorder()
			_, done := startPublicStream(t, f.h, recorder)
			deadline := time.After(time.Second)
			for {
				f.store.mu.Lock()
				queries := f.store.queries
				f.store.mu.Unlock()
				if queries > 0 {
					break
				}
				select {
				case <-deadline:
					t.Fatal("PublicPage query did not start")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			test.stop(f.h)
			waitStreamDone(t, done)
			if got, want := recorder.Body(), "event: close\ndata: {}\n\n"; got != want {
				t.Fatalf("cancelled PublicPage frame = %q, want %q", got, want)
			}
		})
	}
}

func TestPublicStreamPublicPageFailureEndsWithoutFrame(t *testing.T) {
	f := newStreamFixture(t, false)
	f.store.mu.Lock()
	f.store.queryErr = errors.New("query failed")
	f.store.mu.Unlock()
	recorder := newStreamRecorder()
	_, done := startPublicStream(t, f.h, recorder)
	waitStreamDone(t, done)
	if recorder.Body() != "" || recorder.DeadlineCount() != 0 {
		t.Fatalf("PublicPage failure wrote a frame: body=%q deadlines=%d", recorder.Body(), recorder.DeadlineCount())
	}
}

func TestPublicStreamResponseControllerUnwrapsServerWriters(t *testing.T) {
	recorder := newStreamRecorder()
	wrapped := headResponseWriter{&responseLog{ResponseWriter: recorder}}
	if err := writePublicFrame(wrapped, "nodes", []byte(`{"nodes":[]}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.DeadlineCount() != 1 {
		t.Fatalf("nested wrappers hid SetWriteDeadline: count=%d", recorder.DeadlineCount())
	}
}

func TestPublicStreamRoutingValidationAndMetrics(t *testing.T) {
	f := newStreamFixture(t, false)
	post := httptest.NewRecorder()
	f.h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/public/v1/stream", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST response = %d Allow=%q body=%s", post.Code, post.Header().Get("Allow"), post.Body)
	}
	bad := httptest.NewRecorder()
	f.h.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream?limit=0", nil))
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), `"code":"validation_failed"`) {
		t.Fatalf("pagination response = %d body=%s", bad.Code, bad.Body)
	}

	var logs bytes.Buffer
	f.h.accessLog = true
	f.h.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	recorder := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, recorder)
	_ = nextFlush(t, recorder)
	if exposition := f.h.metrics.exposition(); !strings.Contains(exposition, "orbitbeacon_public_streams 1\n") {
		t.Fatalf("open stream gauge missing: %s", exposition)
	}
	cancel()
	waitStreamDone(t, done)
	if exposition := f.h.metrics.exposition(); !strings.Contains(exposition, "orbitbeacon_public_streams 0\n") {
		t.Fatalf("closed stream gauge missing: %s", exposition)
	}
	if !strings.Contains(logs.String(), `"route":"/api/public/v1/stream"`) || !strings.Contains(logs.String(), `"status":200`) {
		t.Fatalf("stream access log = %s", logs.String())
	}
}
