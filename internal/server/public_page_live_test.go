package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"orbitbeacon/internal/server/auth"
)

func disablePublicPageForTest(h *handler) {
	h.publicPageDisabled.Store(true)
	h.nodes.InvalidatePublicSafety()
}

func requirePublicDisabledFrame(t *testing.T, frame map[string]json.RawMessage) {
	t.Helper()
	if string(frame["type"]) != `"error"` || string(frame["code"]) != `"public_page_disabled"` || frame["data"] != nil || frame["server_time"] == nil || frame["update_interval_seconds"] == nil {
		t.Fatalf("invalid public disabled terminal frame: %s", objectJSON(t, frame))
	}
}

func TestPublicPageDisabledDuringSnapshotReturns403(t *testing.T) {
	f := newStreamFixture(t, true)
	f.store.onQuery = func() { disablePublicPageForTest(f.h) }
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"public_page_disabled"`) || strings.Contains(w.Body.String(), "Public node") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("disabled public response permits caching")
	}
}

func TestPublicPageDisableCancelsBlockedInitialProjection(t *testing.T) {
	f := newStreamFixture(t, true)
	entered, canceled := make(chan struct{}), make(chan struct{}, 1)
	var once sync.Once
	f.store.onQuery = func() { once.Do(func() { close(entered) }) }
	f.store.queryWait, f.store.queryCanceled = make(chan struct{}), canceled
	w := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, w)
	defer cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("projection did not start")
	}
	disablePublicPageForTest(f.h)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disable did not cancel projection")
	}
	requirePublicDisabledFrame(t, lastLiveTestFrame(t, nextFlush(t, w)))
	waitStreamDone(t, done)
	if strings.Contains(w.Body(), "Public node") {
		t.Fatal("disabled projection leaked node data")
	}
}

func TestPublicPageDisableTerminatesIdleSSEAndWebSocket(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			f := newStreamFixture(t, true)
			if transport == "sse" {
				w := newStreamRecorder()
				cancel, done := startPublicStream(t, f.h, w)
				defer cancel()
				_ = nextFlush(t, w)
				disablePublicPageForTest(f.h)
				requirePublicDisabledFrame(t, lastLiveTestFrame(t, nextFlush(t, w)))
				waitStreamDone(t, done)
				return
			}
			f.h.origin = "https://beacon.test"
			s := httptest.NewServer(f.h)
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/public/v1/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {f.h.origin}}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			if _, _, err := c.Read(ctx); err != nil {
				t.Fatal(err)
			}
			disablePublicPageForTest(f.h)
			_, body, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var frame map[string]json.RawMessage
			if err := json.Unmarshal(body, &frame); err != nil {
				t.Fatal(err)
			}
			requirePublicDisabledFrame(t, frame)
			if _, _, err := c.Read(ctx); err == nil {
				t.Fatal("disabled websocket remained open")
			}
		})
	}
}

type publicPageBlockedWriter struct {
	*blockedLiveWriter
	once sync.Once
}

func (w *publicPageBlockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { _, _ = w.blockedLiveWriter.Write(p) })
	return 0, errors.New("socket interrupted")
}

func TestPublicPageDisableInterruptsBlockedLiveWrite(t *testing.T) {
	f := newStreamFixture(t, true)
	w := &publicPageBlockedWriter{blockedLiveWriter: &blockedLiveWriter{streamRecorder: newStreamRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil))
	}()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	disablePublicPageForTest(f.h)
	waitStreamDone(t, done)
	if !w.interrupted.Load() {
		t.Fatal("disable did not interrupt blocked snapshot write")
	}
}

type publicPageMarshaler struct{ disable func() }

func (p publicPageMarshaler) MarshalJSON() ([]byte, error) {
	p.disable()
	return []byte(`{"private":"must not escape"}`), nil
}

func TestPublicPageDisableDuringSerializationSendsOnlyTerminalFrame(t *testing.T) {
	f := newStreamFixture(t, false)
	stream := reserveLiveTestStream(t, f.h, "serialize")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var frames []map[string]json.RawMessage
	runLive(f.h, ctx, ctx, stream, liveSource[publicPageMarshaler]{
		snapshot: func(context.Context) (publicPageMarshaler, uint64, error) {
			return publicPageMarshaler{disable: func() { disablePublicPageForTest(f.h) }}, f.h.nodes.PublicVisibilityVersion(), nil
		},
		equal:        func(publicPageMarshaler, publicPageMarshaler) bool { return false },
		authenticate: func(context.Context) error { return f.h.publicPageAccess() },
	}, func(_ context.Context, body []byte) error {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(body, &frame); err != nil {
			return err
		}
		frames = append(frames, frame)
		return nil
	})
	if len(frames) != 1 {
		t.Fatalf("frames=%v", frames)
	}
	requirePublicDisabledFrame(t, frames[0])
}

func TestPublicPageLiveTerminalClassificationIsExact(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{errPublicPageDisabled, "public_page_disabled"}, {auth.ErrUnauthenticated, "unauthenticated"},
		{errors.New("public page is disabled"), ""}, {errors.New("database unavailable"), ""}, {context.Canceled, ""}, {nil, ""},
	} {
		if got := liveTerminalCode(test.err); got != test.want {
			t.Fatalf("error=%v code=%q want=%q", test.err, got, test.want)
		}
	}
}

func TestPublicPageLiveTerminalErrorsAcrossSenderPaths(t *testing.T) {
	for _, path := range []string{"authorize", "snapshot", "snapshot-write", "heartbeat-write"} {
		t.Run(path, func(t *testing.T) {
			f := newStreamFixture(t, false)
			f.h.streamHeartbeat = time.Millisecond
			f.h.streamInterval = time.Hour
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stream := reserveLiveTestStream(t, f.h, path)
			var frames []map[string]json.RawMessage
			runLive(f.h, ctx, ctx, stream, liveSource[int]{
				authenticate: func(context.Context) error {
					if path == "authorize" {
						return errPublicPageDisabled
					}
					return nil
				},
				snapshot: func(context.Context) (int, uint64, error) {
					if path == "snapshot" {
						return 0, 0, errPublicPageDisabled
					}
					return 1, f.h.nodes.PublicVisibilityVersion(), nil
				},
				equal: func(a, b int) bool { return a == b },
			}, func(_ context.Context, body []byte) error {
				var frame map[string]json.RawMessage
				if err := json.Unmarshal(body, &frame); err != nil {
					return err
				}
				if path == "snapshot-write" && string(frame["type"]) == `"snapshot"` || path == "heartbeat-write" && string(frame["type"]) == `"heartbeat"` {
					return errPublicPageDisabled
				}
				frames = append(frames, frame)
				return nil
			})
			if len(frames) == 0 {
				t.Fatal("terminal error was dropped")
			}
			requirePublicDisabledFrame(t, frames[len(frames)-1])
		})
	}
}

func TestPublicPageDisabledAfterProjectionBeforeHTTPSerialization(t *testing.T) {
	f := newStreamFixture(t, true)
	now := f.h.now()
	calls := 0
	f.h.now = func() time.Time {
		calls++
		if calls == 2 {
			disablePublicPageForTest(f.h)
		}
		return now
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"public_page_disabled"`) || strings.Contains(w.Body.String(), "Public node") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}
