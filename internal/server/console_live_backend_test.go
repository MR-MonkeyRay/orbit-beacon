package server

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

type consoleLiveFixture struct {
	h      *handler
	db     *sql.DB
	cookie *http.Cookie
	clock  atomic.Int64
}

func newConsoleLiveFixture(t *testing.T) *consoleLiveFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &consoleLiveFixture{db: db}
	f.clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
	a := auth.NewService(db, auth.Options{Now: now})
	if _, err := a.BootstrapAdministrator(context.Background(), "admin", "private-live-password"); err != nil {
		t.Fatal(err)
	}
	session, err := a.Login(context.Background(), "admin", "private-live-password")
	if err != nil {
		t.Fatal(err)
	}
	f.h, err = NewHandler(Options{Auth: a, Nodes: nodes.NewService(db, nodes.Options{Now: now}), PublicURL: "https://beacon.test", Now: now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	f.cookie = f.h.cookie(session.Token, session.ExpiresAt, 3600)
	f.h.streamInterval = time.Second
	f.h.streamHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() {
		f.h.CloseLiveStreams()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = f.h.WaitLiveStreams(ctx)
		_ = db.Close()
	})
	return f
}

func startConsoleLiveStream(t *testing.T, f *consoleLiveFixture, w http.ResponseWriter) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, "https://beacon.test/api/v1/live/stream", nil).WithContext(ctx)
	r.AddCookie(f.cookie)
	done := make(chan struct{})
	go func() { defer close(done); f.h.ServeHTTP(w, r) }()
	return cancel, done
}

func TestConsoleLiveReauthenticatesHeartbeatAfterSessionRevocationExpiryOrUserDeletion(t *testing.T) {
	for _, mutation := range []string{"revoke", "expire", "delete-user"} {
		t.Run(mutation, func(t *testing.T) {
			f := newConsoleLiveFixture(t)
			w := newStreamRecorder()
			cancel, done := startConsoleLiveStream(t, f, w)
			defer cancel()
			if frame := lastLiveTestFrame(t, nextFlush(t, w)); string(frame["type"]) != `"snapshot"` {
				t.Fatalf("first=%s", w.Body())
			}
			switch mutation {
			case "revoke":
				if _, err := f.db.Exec("DELETE FROM sessions"); err != nil {
					t.Fatal(err)
				}
			case "expire":
				f.clock.Add(int64(auth.SessionLifetime))
			case "delete-user":
				if _, err := f.db.Exec("DELETE FROM users"); err != nil {
					t.Fatal(err)
				}
			}
			waitStreamDone(t, done)
			frame := lastLiveTestFrame(t, w.Body())
			if string(frame["type"]) != `"error"` || string(frame["code"]) != `"unauthenticated"` || frame["data"] != nil {
				t.Fatalf("private stream did not terminate securely: %s", w.Body())
			}
		})
	}
}

func TestConsoleLiveKnownExpiryEndsAsUnauthorizedNotRenew(t *testing.T) {
	f := newConsoleLiveFixture(t)
	expires := time.Unix(0, f.clock.Load()).Add(70 * time.Millisecond)
	if _, err := f.db.Exec("UPDATE sessions SET expires_at=?", expires.UnixNano()); err != nil {
		t.Fatal(err)
	}
	f.h.streamHeartbeat = time.Second
	w := newStreamRecorder()
	cancel, done := startConsoleLiveStream(t, f, w)
	defer cancel()
	_ = nextFlush(t, w)
	waitStreamDone(t, done)
	frame := lastLiveTestFrame(t, w.Body())
	if string(frame["type"]) != `"error"` || string(frame["code"]) != `"unauthenticated"` {
		t.Fatalf("expiry became renewal: %s", w.Body())
	}
}

func TestConsoleLiveReauthenticatesAfterBlockedProjectionBeforePrivateBytes(t *testing.T) {
	for _, transport := range []string{"", "/stream"} {
		t.Run(transport, func(t *testing.T) {
			f := newConsoleLiveFixture(t)
			store := &publicStreamStore{queryWait: make(chan struct{})}
			gate, release := liveTestGate(t)
			store.queryWait = gate
			entered := make(chan struct{}, 1)
			store.onQuery = func() { entered <- struct{}{} }
			db := sql.OpenDB(publicStreamConnector{store: store})
			defer db.Close()
			f.h.nodes = nodes.NewService(db, nodes.Options{})
			w := newStreamRecorder()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest(http.MethodGet, "https://beacon.test/api/v1/live"+transport, nil).WithContext(ctx)
			r.AddCookie(f.cookie)
			done := make(chan struct{})
			go func() { defer close(done); f.h.ServeHTTP(w, r) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("projection did not block")
			}
			if _, err := f.db.Exec("DELETE FROM sessions"); err != nil {
				t.Fatal(err)
			}
			release()
			waitStreamDone(t, done)
			if strings.Contains(w.Body(), `"type":"snapshot"`) {
				t.Fatalf("private data escaped after revocation: %s", w.Body())
			}
			if transport == "" {
				if w.status != http.StatusUnauthorized {
					t.Fatalf("status=%d body=%s", w.status, w.Body())
				}
			} else if frame := lastLiveTestFrame(t, w.Body()); string(frame["code"]) != `"unauthenticated"` {
				t.Fatalf("body=%s", w.Body())
			}
		})
	}
}

func TestConsoleLiveShutdownInterruptsWriterAndJoinsProjection(t *testing.T) {
	t.Run("writer", func(t *testing.T) {
		f := newConsoleLiveFixture(t)
		w := &blockedLiveWriter{streamRecorder: newStreamRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}
		cancel, done := startConsoleLiveStream(t, f, w)
		defer cancel()
		select {
		case <-w.entered:
		case <-time.After(time.Second):
			t.Fatal("writer did not block")
		}
		if err := f.h.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitStreamDone(t, done)
		if !w.interrupted.Load() {
			t.Fatal("shutdown retained blocked private writer")
		}
	})
	t.Run("projection", func(t *testing.T) {
		f := newConsoleLiveFixture(t)
		entered := make(chan struct{}, 1)
		canceled := make(chan struct{}, 1)
		exit, release := liveTestGate(t)
		store := &publicStreamStore{queryWait: make(chan struct{}), queryExitGate: exit, queryCanceled: canceled, onQuery: func() { entered <- struct{}{} }}
		db := sql.OpenDB(publicStreamConnector{store: store})
		defer db.Close()
		f.h.nodes = nodes.NewService(db, nodes.Options{})
		w := newStreamRecorder()
		cancel, done := startConsoleLiveStream(t, f, w)
		defer cancel()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("projection did not block")
		}
		shutdown := make(chan error, 1)
		go func() { shutdown <- f.h.Shutdown(context.Background()) }()
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("shutdown did not cancel projection")
		}
		select {
		case err := <-shutdown:
			t.Fatalf("shutdown did not join projection: %v", err)
		default:
		}
		if _, err := f.h.ingest.Accept(context.Background(), "unused", v1.Report{}); !errors.Is(err, ingest.ErrProtocolUnsupported) {
			t.Fatalf("ingest drained early: %v", err)
		}
		release()
		if err := <-shutdown; err != nil {
			t.Fatal(err)
		}
		waitStreamDone(t, done)
	})
}

func TestConsoleLiveWrappedHTTPSSENonemptyWebSocketHaveSameDataAndExactOrigin(t *testing.T) {
	f := newConsoleLiveFixture(t)
	if _, err := f.h.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "private node"}); err != nil {
		t.Fatal(err)
	}
	f.h.accessLog = true
	s := httptest.NewServer(f.h)
	defer s.Close()
	var frames []map[string]json.RawMessage
	for _, suffix := range []string{"", "/stream"} {
		r, _ := http.NewRequest(http.MethodGet, s.URL+"/api/v1/live"+suffix, nil)
		r.AddCookie(f.cookie)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var frame map[string]json.RawMessage
		if suffix == "" {
			err = json.NewDecoder(response.Body).Decode(&frame)
		} else {
			reader := bufio.NewReader(response.Body)
			for {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					err = readErr
					break
				}
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					err = json.Unmarshal([]byte(strings.TrimSpace(data)), &frame)
					break
				}
			}
		}
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(s.URL, "http") + "/api/v1/live/ws"
	for _, origin := range []string{"", "https://evil.test", "https://beacon.test/"} {
		c, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}, "Cookie": {f.cookie.String()}}})
		if c != nil {
			c.CloseNow()
			t.Fatal("untrusted origin upgraded")
		}
		if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("origin=%q response=%v err=%v", origin, response, err)
		}
	}
	c, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://beacon.test"}, "Cookie": {f.cookie.String()}}})
	if err != nil {
		t.Fatalf("upgrade=%v err=%v", response, err)
	}
	defer c.CloseNow()
	_, body, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	frames = append(frames, frame)
	for _, frame := range frames {
		if string(frame["type"]) != `"snapshot"` || !reflect.DeepEqual(frame["data"], frames[0]["data"]) {
			t.Fatalf("wire mismatch: %s vs %s", frame, frames[0])
		}
	}
	beforeRevocation := f.clock.Load()
	if _, err := f.db.Exec("DELETE FROM sessions"); err != nil {
		t.Fatal(err)
	}
	f.clock.Store(beforeRevocation + 1)
	for {
		_, body, err = c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		frame = nil
		if err := json.Unmarshal(body, &frame); err != nil {
			t.Fatal(err)
		}
		if string(frame["code"]) == `"unauthenticated"` && frame["data"] == nil {
			break
		}
		// A heartbeat already authorized before the durable revocation may be
		// buffered in the socket. Frames built afterward must not stay live.
		var emitted time.Time
		if err := json.Unmarshal(frame["server_time"], &emitted); err != nil {
			t.Fatal(err)
		}
		if string(frame["type"]) != `"heartbeat"` || frame["data"] != nil || emitted.UnixNano() > beforeRevocation {
			t.Fatalf("revoked WS emitted a new live frame: %s", body)
		}
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("revoked WS stayed open")
	}
}

func TestConsoleLiveSharesPublicAdmissionAndRejectsMalformedQueryEncoding(t *testing.T) {
	f := newConsoleLiveFixture(t)
	for _, raw := range []string{"node_id=%zz", "node_id=00000000-0000-4000-8000-000000000001;command=subscribe", "node_id=00000000-0000-4000-8000-000000000001&unexpected=true"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/live", nil)
		r.URL.RawQuery = raw
		r.AddCookie(f.cookie)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d body=%s", raw, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/live/stream", nil)
	r.AddCookie(f.cookie)
	for range 4 {
		reserveLiveTestStream(t, f.h, f.h.publicStreamIP(r))
	}
	for _, path := range []string{"/api/v1/live/stream", "/api/public/v1/stream"} {
		request := r.Clone(context.Background())
		request.URL.Path = path
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, request)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "10" {
			t.Fatalf("shared capacity path=%s status=%d body=%s", path, w.Code, w.Body)
		}
	}
}

func TestConsoleLiveOrdinaryChangesAreCoalescedAndQuietFramesAreHeartbeats(t *testing.T) {
	f := newConsoleLiveFixture(t)
	f.h.streamInterval = 100 * time.Millisecond
	f.h.streamHeartbeat = 20 * time.Millisecond
	w := newStreamRecorder()
	cancel, done := startConsoleLiveStream(t, f, w)
	defer func() { cancel(); waitStreamDone(t, done) }()
	first := lastLiveTestFrame(t, nextFlush(t, w))
	if string(first["type"]) != `"snapshot"` {
		t.Fatalf("first=%s", w.Body())
	}
	if _, err := f.h.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "new inventory"}); err != nil {
		t.Fatal(err)
	}
	quiet := lastLiveTestFrame(t, nextFlush(t, w))
	if string(quiet["type"]) != `"heartbeat"` || quiet["data"] != nil {
		t.Fatalf("ordinary inventory escaped cadence: %s", w.Body())
	}
	deadline := time.After(time.Second)
	for {
		select {
		case body := <-w.flushed:
			frame := lastLiveTestFrame(t, body)
			if string(frame["type"]) == `"snapshot"` {
				var data consoleFleetData
				if err := json.Unmarshal(frame["data"], &data); err != nil {
					t.Fatal(err)
				}
				if len(data.Nodes) != 1 {
					t.Fatalf("new inventory missing: %s", body)
				}
				return
			}
		case <-deadline:
			t.Fatal("ordinary inventory never reached fleet")
		}
	}
}

func TestConsoleLivePrivateWebSocketIsReadOnlyAndReleasesNativeReader(t *testing.T) {
	f := newConsoleLiveFixture(t)
	s := httptest.NewServer(f.h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/v1/live/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://beacon.test"}, "Cookie": {f.cookie.String()}}})
	if err != nil {
		t.Fatalf("upgrade=%v err=%v", response, err)
	}
	defer c.CloseNow()
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"subscribe":"another node"}`))
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("private readonly channel accepted a subscription command")
	}
	if err := f.h.WaitLiveStreams(ctx); err != nil {
		t.Fatalf("private native reader retained lease: %v", err)
	}
}

type consoleFleetBatchConnector struct{ queries *atomic.Int64 }

func (c consoleFleetBatchConnector) Connect(context.Context) (driver.Conn, error) {
	return consoleFleetBatchConn(c), nil
}
func (consoleFleetBatchConnector) Driver() driver.Driver { return publicStreamDriver{} }

type consoleFleetBatchConn struct{ queries *atomic.Int64 }

func (consoleFleetBatchConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (consoleFleetBatchConn) Close() error                        { return nil }
func (consoleFleetBatchConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c consoleFleetBatchConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.queries.Add(1)
	return &consoleFleetBatchRows{}, nil
}

type consoleFleetBatchRows struct{ index int }

func (*consoleFleetBatchRows) Columns() []string {
	return []string{"id", "status", "last_seen", "agent_version", "token_hash", "credential_epoch", "run_generation", "report_session_id"}
}
func (*consoleFleetBatchRows) Close() error { return nil }
func (r *consoleFleetBatchRows) Next(dest []driver.Value) error {
	if r.index == 500 {
		return io.EOF
	}
	copy(dest, []driver.Value{fmt.Sprintf("00000000-0000-4000-8000-%012d", r.index), "pending", nil, "", nil, int64(0), "0", ""})
	r.index++
	return nil
}

func TestConsoleLiveFiveHundredNodeFleetUsesOneBatchRead(t *testing.T) {
	f := newConsoleLiveFixture(t)
	var queries atomic.Int64
	db := sql.OpenDB(consoleFleetBatchConnector{queries: &queries})
	defer db.Close()
	f.h.nodes = nodes.NewService(db, nodes.Options{})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/live", nil)
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var frame liveFrame[consoleFleetData]
	if err := json.Unmarshal(w.Body.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Data == nil || len(frame.Data.Nodes) != 500 || queries.Load() != 1 {
		t.Fatalf("fleet=%+v queries=%d", frame.Data, queries.Load())
	}
	for _, node := range frame.Data.Nodes {
		if node.Presence != "pending" {
			t.Fatalf("batch record lost presence: %+v", node)
		}
	}
}

func TestConsoleLiveIdentitySafetyCancelsProjectionWithoutSendingSnapshot(t *testing.T) {
	f := newConsoleLiveFixture(t)
	entered := make(chan struct{}, 1)
	store := &publicStreamStore{queryWait: make(chan struct{}), onQuery: func() { entered <- struct{}{} }}
	db := sql.OpenDB(publicStreamConnector{store: store})
	defer db.Close()
	f.h.nodes = nodes.NewService(db, nodes.Options{})
	w := newStreamRecorder()
	cancel, done := startConsoleLiveStream(t, f, w)
	defer cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("projection did not block")
	}
	f.h.nodes.InvalidatePublicSafety()
	waitStreamDone(t, done)
	frame := lastLiveTestFrame(t, w.Body())
	if string(frame["type"]) != `"close"` || string(frame["code"]) != `"renew"` || strings.Contains(w.Body(), `"type":"snapshot"`) {
		t.Fatalf("identity race escaped: %s", w.Body())
	}
}

func TestConsoleLiveDetailWireDistinguishesAbsentSampleFromDeletionAcrossTransports(t *testing.T) {
	f := newConsoleLiveFixture(t)
	node, err := f.h.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "detail"})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(f.h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, deleted := range []bool{false, true} {
		if deleted {
			if err := f.h.nodes.Delete(context.Background(), node.ID, true); err != nil {
				t.Fatal(err)
			}
		}
		for _, transport := range []string{"http", "sse", "ws"} {
			t.Run(fmt.Sprintf("deleted=%t/%s", deleted, transport), func(t *testing.T) {
				var body []byte
				if transport == "ws" {
					c, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/v1/live/ws?node_id="+node.ID, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://beacon.test"}, "Cookie": {f.cookie.String()}}})
					if err != nil {
						t.Fatalf("upgrade=%v err=%v", response, err)
					}
					_, body, err = c.Read(ctx)
					c.CloseNow()
					if err != nil {
						t.Fatal(err)
					}
				} else {
					path := "/api/v1/live"
					if transport == "sse" {
						path += "/stream"
					}
					r, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path+"?node_id="+node.ID, nil)
					r.AddCookie(f.cookie)
					response, err := http.DefaultClient.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					if transport == "http" {
						body, err = io.ReadAll(response.Body)
					} else {
						reader := bufio.NewReader(response.Body)
						for {
							line, readErr := reader.ReadString('\n')
							if readErr != nil {
								err = readErr
								break
							}
							if data, ok := strings.CutPrefix(line, "data: "); ok {
								body = []byte(strings.TrimSpace(data))
								break
							}
						}
					}
					response.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				var frame struct {
					Type string                     `json:"type"`
					Data map[string]json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(body, &frame); err != nil {
					t.Fatal(err)
				}
				if frame.Type != "snapshot" || len(frame.Data) != 3 || (string(frame.Data["node"]) == "null") != deleted || string(frame.Data["latest"]) != "null" || string(frame.Data["runtime"]) != "null" {
					t.Fatalf("detail=%s", body)
				}
				if transport != "http" {
					if err := f.h.WaitLiveStreams(ctx); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestConsoleLiveHTTPReauthenticatesAfterLogoutPasswordChangeAndUserDeletion(t *testing.T) {
	for _, mutation := range []string{"logout", "password", "delete-user", "expiry"} {
		t.Run(mutation, func(t *testing.T) {
			f := newConsoleLiveFixture(t)
			session, err := f.h.auth.CurrentSession(context.Background(), f.cookie.Value)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "logout":
				r := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
				r.AddCookie(f.cookie)
				r.Header.Set("Origin", "https://beacon.test")
				r.Header.Set("X-CSRF-Token", session.CSRFToken)
				w := httptest.NewRecorder()
				f.h.ServeHTTP(w, r)
				if w.Code != http.StatusNoContent {
					t.Fatalf("logout=%d %s", w.Code, w.Body)
				}
			case "password":
				if err := f.h.auth.ChangePassword(context.Background(), session.User.ID, "private-live-password", "replacement-private-password"); err != nil {
					t.Fatal(err)
				}
			case "delete-user":
				if _, err := f.db.Exec("DELETE FROM users"); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				f.clock.Add(int64(auth.SessionLifetime))
			}
			for _, path := range []string{"/api/v1/live", "/api/v1/live/stream", "/api/v1/live/ws"} {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.AddCookie(f.cookie)
				r.Header.Set("Origin", "https://beacon.test")
				w := httptest.NewRecorder()
				f.h.ServeHTTP(w, r)
				if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), `"data"`) {
					t.Fatalf("revoked %s response=%d %s", path, w.Code, w.Body)
				}
			}
		})
	}
}

func TestConsoleLiveShutdownClosesActualHijackedBlockedWebSocketWriter(t *testing.T) {
	f := newConsoleLiveFixture(t)
	entered := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.h.ServeHTTP(blockedUpgradeWriter{ResponseWriter: w, entered: entered}, r)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/v1/live/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://beacon.test"}, "Cookie": {f.cookie.String()}}})
	if err != nil {
		t.Fatalf("upgrade=%v err=%v", response, err)
	}
	defer c.CloseNow()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("private WS did not encounter actual socket backpressure")
	}
	if err := f.h.Shutdown(ctx); err != nil {
		t.Fatalf("private hijacked writer survived drain: %v", err)
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("private WS remained open after drain")
	}
}

func TestConsoleLiveExpiryCancelsBlockedProjectionAndNeverSendsPrivateSnapshot(t *testing.T) {
	f := newConsoleLiveFixture(t)
	expires := time.Unix(0, f.clock.Load()).Add(60 * time.Millisecond)
	if _, err := f.db.Exec("UPDATE sessions SET expires_at=?", expires.UnixNano()); err != nil {
		t.Fatal(err)
	}
	store := &publicStreamStore{queryWait: make(chan struct{})}
	db := sql.OpenDB(publicStreamConnector{store: store})
	defer db.Close()
	f.h.nodes = nodes.NewService(db, nodes.Options{})
	w := newStreamRecorder()
	cancel, done := startConsoleLiveStream(t, f, w)
	defer cancel()
	waitStreamDone(t, done)
	frame := lastLiveTestFrame(t, w.Body())
	if string(frame["type"]) != `"error"` || string(frame["code"]) != `"unauthenticated"` || strings.Contains(w.Body(), `"type":"snapshot"`) {
		t.Fatalf("expired blocked projection escaped: %s", w.Body())
	}
}

func TestConsoleLiveRejectedWebSocketHandshakeNeverAcquiresPrivateData(t *testing.T) {
	f := newConsoleLiveFixture(t)
	s := httptest.NewServer(f.h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(s.URL, "http") + "/api/v1/live/ws"
	for _, test := range []struct {
		query   string
		headers http.Header
		status  int
	}{
		{"", http.Header{"Origin": {"https://beacon.test"}}, http.StatusUnauthorized},
		{"?node_id=bad", http.Header{"Origin": {"https://beacon.test"}, "Cookie": {f.cookie.String()}}, http.StatusBadRequest},
		{"", http.Header{"Origin": {"https://beacon.test", "https://beacon.test"}, "Cookie": {f.cookie.String()}}, http.StatusForbidden},
	} {
		c, response, err := websocket.Dial(ctx, url+test.query, &websocket.DialOptions{HTTPHeader: test.headers})
		if c != nil {
			c.CloseNow()
			t.Fatal("invalid private WS upgraded")
		}
		if err == nil || response == nil || response.StatusCode != test.status {
			t.Fatalf("response=%v err=%v want=%d", response, err, test.status)
		}
	}
	if err := f.h.WaitLiveStreams(ctx); err != nil {
		t.Fatal(err)
	}
}

// A real interrupted socket rejects subsequent terminal writes immediately.
type expiredConsoleWriter struct{ *blockedLiveWriter }

func (w expiredConsoleWriter) Write(body []byte) (int, error) {
	if w.interrupted.Load() {
		return 0, errors.New("socket closed")
	}
	return w.blockedLiveWriter.Write(body)
}

func TestConsoleLiveKnownExpiryInterruptsActivePrivateSSEWriter(t *testing.T) {
	f := newConsoleLiveFixture(t)
	expires := time.Unix(0, f.clock.Load()).Add(60 * time.Millisecond)
	if _, err := f.db.Exec("UPDATE sessions SET expires_at=?", expires.UnixNano()); err != nil {
		t.Fatal(err)
	}
	w := expiredConsoleWriter{&blockedLiveWriter{streamRecorder: newStreamRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}}
	cancel, done := startConsoleLiveStream(t, f, w)
	defer cancel()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("private writer never started")
	}
	waitStreamDone(t, done)
	if !w.interrupted.Load() {
		t.Fatal("session expiry did not interrupt private bytes")
	}
}

func TestConsoleLiveMaximumLifetimeRenewsBeforeLongSessionExpiry(t *testing.T) {
	f := newConsoleLiveFixture(t)
	f.h.streamLifetime = 50 * time.Millisecond
	f.h.streamHeartbeat = time.Second
	w := newStreamRecorder()
	cancel, done := startConsoleLiveStream(t, f, w)
	defer cancel()
	_ = nextFlush(t, w)
	waitStreamDone(t, done)
	frame := lastLiveTestFrame(t, w.Body())
	if string(frame["type"]) != `"close"` || string(frame["code"]) != `"renew"` {
		t.Fatalf("normal lease lifetime treated as session expiry: %s", w.Body())
	}
}
