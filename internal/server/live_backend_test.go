package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
)

func liveTestGate(t *testing.T) (chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return gate, release
}

func objectJSON(t *testing.T, object map[string]json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func reserveLiveTestStream(t *testing.T, h *handler, ip string) *liveStream {
	t.Helper()
	stream, ok := h.streams.acquire(ip)
	if !ok {
		t.Fatal("failed to reserve stream capacity")
	}
	t.Cleanup(stream.release)
	return stream
}

func lastLiveTestFrame(t *testing.T, text string) map[string]json.RawMessage {
	t.Helper()
	var last map[string]json.RawMessage
	for _, event := range strings.Split(text, "\n\n") {
		if strings.TrimSpace(event) == "" {
			continue
		}
		var data string
		for _, line := range strings.Split(event, "\n") {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data += strings.TrimSpace(value)
			}
		}
		last = nil
		if err := json.Unmarshal([]byte(data), &last); err != nil {
			t.Fatalf("frame=%q: %v", event, err)
		}
	}
	return last
}

func TestPublicLiveHTTPRejectsVisibilityChurn(t *testing.T) {
	f := newStreamFixture(t, true)
	f.store.onQuery = func() { f.h.nodes.InvalidatePublic() }
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "10" || !strings.Contains(w.Body.String(), `"code":"snapshot_busy"`) || strings.Contains(w.Body.String(), "Public node") {
		t.Fatalf("racing visibility escaped: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
	f.store.mu.Lock()
	queries := f.store.queries
	f.store.mu.Unlock()
	if queries != 3 {
		t.Fatalf("queries=%d want exactly three bounded attempts", queries)
	}
}

func TestPublicLiveStreamRejectsVisibilityChurn(t *testing.T) {
	f := newStreamFixture(t, true)
	f.store.onQuery = func() { f.h.nodes.InvalidatePublic() }
	w := newStreamRecorder()
	_, done := startPublicStream(t, f.h, w)
	waitStreamDone(t, done)
	if strings.Contains(w.Body(), "Public node") || strings.Contains(w.Body(), `"type":"snapshot"`) {
		t.Fatalf("churn emitted a snapshot: %s", w.Body())
	}
	f.store.mu.Lock()
	queries := f.store.queries
	f.store.mu.Unlock()
	if queries != 3 {
		t.Fatalf("queries=%d want three", queries)
	}
}

func TestPublicLiveIntervalMetadataAndQuietProjectionCadence(t *testing.T) {
	for _, seconds := range []int{2, 10, 30, 60} {
		t.Run((time.Duration(seconds) * time.Second).String(), func(t *testing.T) {
			f := newStreamFixture(t, false)
			f.h.webUpdateIntervalSeconds = seconds
			f.h.streamInterval = time.Second
			f.h.streamHeartbeat = 15 * time.Millisecond
			var millis atomic.Int64
			base := f.h.now()
			f.h.now = func() time.Time { return base.Add(time.Duration(millis.Load()) * time.Millisecond) }
			w := newStreamRecorder()
			cancel, done := startPublicStream(t, f.h, w)
			defer func() { cancel(); waitStreamDone(t, done) }()
			first := lastLiveTestFrame(t, nextFlush(t, w))
			var got int
			_ = json.Unmarshal(first["update_interval_seconds"], &got)
			if got != seconds {
				t.Fatalf("interval=%d want=%d", got, seconds)
			}
			millis.Store(1000)
			heartbeat := lastLiveTestFrame(t, nextFlush(t, w))
			if string(heartbeat["type"]) != `"heartbeat"` || heartbeat["data"] != nil || heartbeat["code"] != nil {
				t.Fatalf("quiet frame=%s", w.Body())
			}
			f.store.mu.Lock()
			queries := f.store.queries
			f.store.mu.Unlock()
			if queries != 1 {
				t.Fatalf("heartbeat recalculated projection early: queries=%d", queries)
			}
		})
	}
}

func TestPublicLiveWebSocketRejectsParametersAndReleasesReadOnlyClient(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.origin = "https://beacon.test"
	s := httptest.NewServer(f.h)
	defer s.Close()
	url := "ws" + strings.TrimPrefix(s.URL, "http") + "/api/public/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {f.h.origin}}}
	bad, response, err := websocket.Dial(ctx, url+"?limit=0", opts)
	if bad != nil {
		bad.CloseNow()
		t.Fatal("invalid pagination upgraded")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid params response=%v err=%v", response, err)
	}
	for _, payload := range []string{"write command", strings.Repeat("x", 1025)} {
		c, _, err := websocket.Dial(ctx, url, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Read(ctx); err != nil {
			t.Fatal(err)
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(payload))
		if _, _, err := c.Read(ctx); err == nil {
			t.Fatal("read-only channel accepted an application message")
		}
		c.CloseNow()
		if err := f.h.WaitLiveStreams(ctx); err != nil {
			t.Fatalf("capacity retained after read-only close: %v", err)
		}
	}
}

type blockedLiveWriter struct {
	*streamRecorder
	entered     chan struct{}
	unblock     chan struct{}
	interrupted atomic.Bool
}

func (w *blockedLiveWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.unblock
	return 0, errors.New("socket interrupted")
}
func (w *blockedLiveWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) && w.interrupted.CompareAndSwap(false, true) {
		close(w.unblock)
	}
	return w.streamRecorder.SetWriteDeadline(deadline)
}

func TestPublicLiveShutdownInterruptsWriterAndWaitsForExit(t *testing.T) {
	f := newStreamFixture(t, false)
	w := &blockedLiveWriter{streamRecorder: newStreamRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil))
	}()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not block")
	}
	f.h.CloseLiveStreams()
	f.h.CloseLiveStreams()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.h.WaitLiveStreams(ctx); err != nil {
		t.Fatal(err)
	}
	waitStreamDone(t, done)
	if !w.interrupted.Load() {
		t.Fatal("shutdown did not interrupt the active write")
	}
}

func TestPublicLiveOrdinaryChangesAreThrottledAndQuietPagesAreNotRepeated(t *testing.T) {
	f := newStreamFixture(t, true)
	f.h.streamInterval = 60 * time.Millisecond
	f.h.streamHeartbeat = 300 * time.Millisecond
	var seconds atomic.Int64
	base := f.h.now()
	f.h.now = func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	w := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, w)
	defer func() { cancel(); waitStreamDone(t, done) }()
	_ = nextFlush(t, w)
	changed := time.Now()
	f.store.mu.Lock()
	f.store.name = "New public text"
	f.store.mu.Unlock()
	seconds.Store(10)
	select {
	case frame := <-w.flushed:
		t.Fatalf("ordinary update bypassed the configured interval: %s", frame)
	case <-time.After(25 * time.Millisecond):
	}
	frame := nextFlush(t, w)
	if time.Since(changed) < 50*time.Millisecond || !strings.Contains(frame, "New public text") || string(lastLiveTestFrame(t, frame)["type"]) != `"snapshot"` {
		t.Fatalf("ordinary change did not coalesce: %s", frame)
	}
	seconds.Store(20)
	select {
	case frame := <-w.flushed:
		t.Fatalf("unchanged generated_at caused a full snapshot: %s", frame)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublicLiveInitialWriteCannotMissVisibilitySubscription(t *testing.T) {
	f := newStreamFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := reserveLiveTestStream(t, f.h, "gap-test")
	var frames []map[string]json.RawMessage
	f.h.runPublicLive(ctx, ctx, stream, 50, 0, func(_ context.Context, body []byte) error {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(body, &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
		if len(frames) == 1 {
			f.h.nodes.InvalidatePublicSafety()
		}
		return nil
	})
	if len(frames) != 2 || string(frames[1]["type"]) != `"close"` || string(frames[1]["code"]) != `"renew"` {
		t.Fatalf("visibility change during initial write was missed: %+v", frames)
	}
}

func TestPublicLiveRegistryDeadlineForcesSocketAndReleaseIsIdempotent(t *testing.T) {
	f := newStreamFixture(t, false)
	stream := reserveLiveTestStream(t, f.h, "force-test")
	var forced atomic.Bool
	stream.setWriter(nil, func() { forced.Store(true) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.h.WaitLiveStreams(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("deadline=%v", err)
	}
	if !forced.Load() {
		t.Fatal("deadline left the socket open")
	}
	stream.release()
	stream.release()
	if err := f.h.WaitLiveStreams(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.h.streams.mu.Lock()
	remaining := f.h.streams.total
	f.h.streams.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("release count=%d", remaining)
	}
}

func TestPublicLiveWebSocketSharesCapacityAndShutdownRegistry(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.origin = "https://beacon.test"
	s := httptest.NewServer(f.h)
	defer s.Close()
	url := "ws" + strings.TrimPrefix(s.URL, "http") + "/api/public/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {f.h.origin}}}
	var connections []*websocket.Conn
	for range 4 {
		c, response, err := websocket.Dial(ctx, url, opts)
		if err != nil {
			t.Fatalf("upgrade response=%v err=%v", response, err)
		}
		connections = append(connections, c)
		defer c.CloseNow()
		if _, _, err := c.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	denied, response, err := websocket.Dial(ctx, url, opts)
	if denied != nil {
		denied.CloseNow()
		t.Fatal("fifth WS bypassed the shared per-IP limit")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("capacity response=%v err=%v", response, err)
	}
	f.h.CloseLiveStreams()
	for _, c := range connections {
		_, body, err := c.Read(ctx)
		if err == nil {
			var frame struct {
				Type string `json:"type"`
				Code string `json:"code"`
			}
			if err := json.Unmarshal(body, &frame); err != nil || frame.Type != "close" || frame.Code != "shutdown" {
				t.Fatalf("shutdown frame=%s err=%v", body, err)
			}
			if _, _, err := c.Read(ctx); err == nil {
				t.Fatal("shutdown left WS open")
			}
		}
	}
	if err := f.h.WaitLiveStreams(ctx); err != nil {
		t.Fatal(err)
	}
	if f.h.metrics.publicStreams.Load() != 0 {
		t.Fatal("public stream metric did not include/release WS")
	}
}

func TestPublicLiveShutdownWaitsForQueryExitBeforeIngest(t *testing.T) {
	f := newStreamFixture(t, false)
	queryStarted := make(chan struct{}, 1)
	queryCanceled := make(chan struct{}, 1)
	releaseQuery, release := liveTestGate(t)
	f.store.onQuery = func() { queryStarted <- struct{}{} }
	f.store.queryWait = make(chan struct{})
	f.store.queryCanceled = queryCanceled
	f.store.queryExitGate = releaseQuery
	w := newStreamRecorder()
	_, done := startPublicStream(t, f.h, w)
	select {
	case <-queryStarted:
	case <-time.After(time.Second):
		t.Fatal("query did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- f.h.Shutdown(ctx) }()
	select {
	case <-queryCanceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the query")
	}
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown returned before query exit: %v", err)
	default:
	}
	_, err := f.h.ingest.Accept(context.Background(), "unused", v1.Report{})
	if !errors.Is(err, ingest.ErrProtocolUnsupported) {
		t.Fatalf("ingest drained while a public query remained active: %v", err)
	}
	release()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown did not finish after query exited")
	}
	waitStreamDone(t, done)
	_, err = f.h.ingest.Accept(context.Background(), "unused", v1.Report{})
	if !errors.Is(err, ingest.ErrServiceClosed) {
		t.Fatalf("ingest did not drain after streams: %v", err)
	}
}

func TestPublicLiveFailedDrainDoesNotStartIngestShutdown(t *testing.T) {
	f := newStreamFixture(t, false)
	queryStarted := make(chan struct{}, 1)
	releaseQuery, release := liveTestGate(t)
	f.store.onQuery = func() { queryStarted <- struct{}{} }
	f.store.queryWait = make(chan struct{})
	f.store.queryExitGate = releaseQuery
	w := newStreamRecorder()
	_, done := startPublicStream(t, f.h, w)
	select {
	case <-queryStarted:
	case <-time.After(time.Second):
		t.Fatal("query did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.h.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown=%v", err)
	}
	_, err := f.h.ingest.Accept(context.Background(), "unused", v1.Report{})
	if !errors.Is(err, ingest.ErrProtocolUnsupported) {
		t.Fatalf("deadline started ingestion drain before stream exit: %v", err)
	}
	release()
	waitStreamDone(t, done)
	if err := f.h.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPublicLiveVisibilityCancelsAnEstablishedBlockedQuery(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.streamInterval = 40 * time.Millisecond
	var seconds atomic.Int64
	base := f.h.now()
	f.h.now = func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	w := newStreamRecorder()
	_, done := startPublicStream(t, f.h, w)
	_ = nextFlush(t, w)
	queryStarted := make(chan struct{}, 1)
	f.store.mu.Lock()
	f.store.onQuery = func() { queryStarted <- struct{}{} }
	f.store.queryWait = make(chan struct{})
	f.store.mu.Unlock()
	seconds.Store(10)
	select {
	case <-queryStarted:
	case <-time.After(time.Second):
		t.Fatal("second query did not start")
	}
	f.h.nodes.InvalidatePublicSafety()
	waitStreamDone(t, done)
	frame := lastLiveTestFrame(t, w.Body())
	if string(frame["type"]) != `"close"` || string(frame["code"]) != `"renew"` {
		t.Fatalf("blocked query did not close for safety: %s", w.Body())
	}
}

func TestPublicLiveWrappedHTTPSSENonemptyWebSocketHaveSameData(t *testing.T) {
	f := newStreamFixture(t, true)
	f.h.origin = "https://beacon.test"
	f.h.accessLog = true
	s := httptest.NewServer(f.h)
	defer s.Close()
	response, err := http.Get(s.URL + "/api/public/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	var httpFrame liveFrame[ingest.PublicPage]
	if err := json.NewDecoder(response.Body).Decode(&httpFrame); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = http.Get(s.URL + "/api/public/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	var event, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data += strings.TrimSpace(value)
		}
	}
	response.Body.Close()
	if event != "message" {
		t.Fatalf("SSE event=%q", event)
	}
	var sseFrame liveFrame[ingest.PublicPage]
	if err := json.Unmarshal([]byte(data), &sseFrame); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, upgraded, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/public/v1/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {f.h.origin}}})
	if err != nil {
		t.Fatalf("wrapped upgrade response=%v err=%v", upgraded, err)
	}
	defer c.CloseNow()
	_, body, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wsFrame liveFrame[ingest.PublicPage]
	if err := json.Unmarshal(body, &wsFrame); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []liveFrame[ingest.PublicPage]{httpFrame, sseFrame, wsFrame} {
		if frame.Type != "snapshot" || frame.Code != "" || frame.Data == nil || frame.ServerTime.IsZero() || frame.UpdateIntervalSeconds != 10 {
			t.Fatalf("invalid live frame: %+v", frame)
		}
		if len(frame.Data.Nodes) != 1 || frame.Data.Nodes[0].Name != "Public node" {
			t.Fatalf("missing public projection: %+v", frame.Data)
		}
		if !publicPagesEqual(*httpFrame.Data, *frame.Data) || !httpFrame.Data.GeneratedAt.Equal(frame.Data.GeneratedAt) {
			t.Fatalf("transport-specific data: http=%+v other=%+v", httpFrame.Data, frame.Data)
		}
	}
}

func TestPublicLiveWebSocketStopsBeforePayloadOnThreeVisibilityRaces(t *testing.T) {
	f := newStreamFixture(t, true)
	f.h.origin = "https://beacon.test"
	f.store.onQuery = func() { f.h.nodes.InvalidatePublic() }
	s := httptest.NewServer(f.h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/public/v1/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {f.h.origin}}})
	if err != nil {
		t.Fatalf("upgrade response=%v err=%v", response, err)
	}
	defer c.CloseNow()
	if _, body, err := c.Read(ctx); err == nil {
		t.Fatalf("racing projection escaped on WS: %s", body)
	}
	if err := f.h.WaitLiveStreams(ctx); err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	queries := f.store.queries
	f.store.mu.Unlock()
	if queries != 3 {
		t.Fatalf("visibility attempts=%d want=3", queries)
	}
}

func TestPublicLiveShutdownWaitsForBlockedProjectionBeforeIngest(t *testing.T) {
	f := newStreamFixture(t, false)
	base := f.h.now()
	projectionEntered := make(chan struct{}, 1)
	releaseProjection, release := liveTestGate(t)
	f.h.now = func() time.Time {
		select {
		case projectionEntered <- struct{}{}:
		default:
		}
		<-releaseProjection
		return base
	}
	w := newStreamRecorder()
	_, done := startPublicStream(t, f.h, w)
	select {
	case <-projectionEntered:
	case <-time.After(time.Second):
		t.Fatal("projection did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- f.h.Shutdown(ctx) }()
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown passed a blocked projection: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	_, err := f.h.ingest.Accept(context.Background(), "unused", v1.Report{})
	if !errors.Is(err, ingest.ErrProtocolUnsupported) {
		t.Fatalf("ingest drained while projection remained active: %v", err)
	}
	release()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("projection exit did not release shutdown")
	}
	waitStreamDone(t, done)
	_, err = f.h.ingest.Accept(context.Background(), "unused", v1.Report{})
	if !errors.Is(err, ingest.ErrServiceClosed) {
		t.Fatalf("ingest not drained after projection exit: %v", err)
	}
}

type blockedLiveSocket struct {
	net.Conn
	entered   chan struct{}
	unblock   chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

func (c *blockedLiveSocket) Write([]byte) (int, error) {
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.unblock
	return 0, net.ErrClosed
}

func (c *blockedLiveSocket) Close() error {
	c.closeOnce.Do(func() { close(c.unblock) })
	return c.Conn.Close()
}

type blockedUpgradeWriter struct {
	http.ResponseWriter
	entered chan struct{}
}

func (w blockedUpgradeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w blockedUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err := rw.Writer.Flush(); err != nil {
		c.Close()
		return nil, nil, err
	}
	blocked := &blockedLiveSocket{Conn: c, entered: w.entered, unblock: make(chan struct{})}
	return blocked, bufio.NewReadWriter(rw.Reader, bufio.NewWriter(blocked)), nil
}

func TestPublicLiveShutdownClosesActualHijackedBlockedWebSocketWriter(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.origin = "https://beacon.test"
	entered := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.h.ServeHTTP(blockedUpgradeWriter{ResponseWriter: w, entered: entered}, r)
	}))
	defer s.Close()
	defer f.h.CloseLiveStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/public/v1/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {f.h.origin}}})
	if err != nil {
		t.Fatalf("upgrade response=%v err=%v", response, err)
	}
	defer c.CloseNow()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("actual WS writer did not encounter backpressure")
	}
	f.h.CloseLiveStreams()
	if err := f.h.WaitLiveStreams(ctx); err != nil {
		t.Fatalf("hijacked socket survived shutdown: %v", err)
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("blocked WS remained open after stream drain")
	}
}

func TestPublicLiveLifetimeInterruptsAnActiveSSEWrite(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.streamLifetime = 25 * time.Millisecond
	w := &blockedLiveWriter{streamRecorder: newStreamRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil))
	}()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	waitStreamDone(t, done)
	if !w.interrupted.Load() {
		t.Fatal("maximum stream lifetime did not interrupt the writer")
	}
}

func TestPublicLiveRegistryDoesNotHoldAdmissionLockDuringInterrupt(t *testing.T) {
	f := newStreamFixture(t, false)
	stream := reserveLiveTestStream(t, f.h, "callback-test")
	called := make(chan struct{})
	stream.setWriter(nil, func() {
		f.h.streams.mu.Lock()
		defer f.h.streams.mu.Unlock()
		close(called)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- f.h.WaitLiveStreams(ctx) }()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("interrupt ran under the admission lock")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("force-close did not return")
	}
}

func TestPublicLiveSnapshotAndQueryCadenceRespectsConfiguredIntervals(t *testing.T) {
	for _, seconds := range []int{2, 10, 30, 60} {
		f := newStreamFixture(t, false)
		f.h.webUpdateIntervalSeconds = seconds
		if f.h.liveSeconds() != seconds {
			t.Fatalf("wire interval=%d", f.h.liveSeconds())
		}
		f.h.streamInterval = 0
		if f.h.liveInterval() != time.Duration(seconds)*time.Second {
			t.Fatalf("projection interval=%v want=%ds", f.h.liveInterval(), seconds)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		stream := reserveLiveTestStream(t, f.h, "cadence-test")
		var frames int
		f.h.runPublicLive(ctx, ctx, stream, 50, 0, func(_ context.Context, body []byte) error {
			var frame struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(body, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Type == "snapshot" {
				frames++
			}
			return nil
		})
		cancel()
		f.store.mu.Lock()
		queries := f.store.queries
		f.store.mu.Unlock()
		if frames != 1 || queries != 1 {
			t.Fatalf("configured=%ds frames=%d queries=%d before interval elapsed", seconds, frames, queries)
		}
	}
}

func TestPublicLiveRejectsSnapshotInvalidatedByFrameClock(t *testing.T) {
	f := newStreamFixture(t, true)
	base := f.h.now()
	var calls atomic.Int64
	f.h.now = func() time.Time {
		if calls.Add(1) == 2 {
			f.h.nodes.InvalidatePublicSafety()
		}
		return base
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"snapshot_busy"`) || strings.Contains(w.Body.String(), "Public node") {
		t.Fatalf("known invalidation between projection and outbound write escaped: status=%d body=%s", w.Code, w.Body)
	}
}

func TestPublicLiveKnownInvalidationAtStreamFrameClockDoesNotSendData(t *testing.T) {
	f := newStreamFixture(t, true)
	base := f.h.now()
	var calls atomic.Int64
	f.h.now = func() time.Time {
		if calls.Add(1) == 2 {
			f.h.nodes.InvalidatePublicSafety()
		}
		return base
	}
	w := newStreamRecorder()
	_, done := startPublicStream(t, f.h, w)
	waitStreamDone(t, done)
	if strings.Contains(w.Body(), "Public node") || strings.Contains(w.Body(), `"type":"snapshot"`) {
		t.Fatalf("known invalid snapshot escaped: %s", w.Body())
	}
}

func TestPublicLiveHTTPRetriesOneVisibilityRaceAndEmitsOnlyCurrentData(t *testing.T) {
	f := newStreamFixture(t, true)
	var once sync.Once
	f.store.onQuery = func() {
		once.Do(func() {
			f.store.mu.Lock()
			f.store.visible = false
			f.store.mu.Unlock()
			f.h.nodes.InvalidatePublicSafety()
		})
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("transient race did not converge: status=%d body=%s", w.Code, w.Body)
	}
	page := decodeTransportSnapshot(t, w.Body.Bytes())
	if len(page.Nodes) != 0 || strings.Contains(w.Body.String(), "Public node") {
		t.Fatalf("withdrawn page escaped after retry: %s", w.Body)
	}
}

type blockedHandshakeWriter struct {
	*streamRecorder
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (w *blockedHandshakeWriter) WriteHeader(status int) {
	if status == http.StatusSwitchingProtocols {
		close(w.entered)
		<-w.unblock
	}
	w.streamRecorder.WriteHeader(status)
}
func (w *blockedHandshakeWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.once.Do(func() { close(w.unblock) })
	}
	return w.streamRecorder.SetWriteDeadline(deadline)
}
func (w *blockedHandshakeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("interrupted handshake")
}

func TestPublicLiveShutdownInterruptsPendingUpgradeBeforeSocketRegistration(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.origin = "https://beacon.test"
	w := &blockedHandshakeWriter{streamRecorder: newStreamRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}
	r := httptest.NewRequest(http.MethodGet, "/api/public/v1/ws", nil)
	r.Header.Set("Origin", f.h.origin)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	done := make(chan struct{})
	go func() { defer close(done); f.h.ServeHTTP(w, r) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("upgrade did not reach its blocked header")
	}
	f.h.CloseLiveStreams()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.h.WaitLiveStreams(ctx); err != nil {
		t.Fatalf("pending upgrade survived stream shutdown: %v", err)
	}
	waitStreamDone(t, done)
}

type liveDeadlineConn struct {
	net.Conn
	cleared chan struct{}
	once    sync.Once
}

func (c *liveDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		c.once.Do(func() { close(c.cleared) })
	}
	return c.Conn.SetWriteDeadline(deadline)
}
func (c *liveDeadlineConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		c.once.Do(func() { close(c.cleared) })
	}
	return c.Conn.SetDeadline(deadline)
}

type liveDeadlineUpgradeWriter struct {
	http.ResponseWriter
	cleared chan struct{}
}

func (w liveDeadlineUpgradeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w liveDeadlineUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &liveDeadlineConn{Conn: c, cleared: w.cleared}, rw, nil
}

func TestPublicLiveWebSocketClearsHTTPHandshakeDeadlineAfterHijack(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.origin = "https://beacon.test"
	cleared := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.h.ServeHTTP(liveDeadlineUpgradeWriter{ResponseWriter: w, cleared: cleared}, r)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/public/v1/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {f.h.origin}}})
	if err != nil {
		t.Fatalf("upgrade response=%v err=%v", response, err)
	}
	defer c.CloseNow()
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleared:
	case <-ctx.Done():
		t.Fatal("HTTP handshake deadline would kill an idle upgraded socket")
	}
}
