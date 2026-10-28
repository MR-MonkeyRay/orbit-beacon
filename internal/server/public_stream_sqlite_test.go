package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
)

type sqliteStreamRecorder struct {
	*httptest.ResponseRecorder
	frames chan string
	offset int
}

func (w *sqliteStreamRecorder) SetWriteDeadline(time.Time) error { return nil }

func (w *sqliteStreamRecorder) Flush() {
	w.ResponseRecorder.Flush()
	body := w.Body.String()
	w.frames <- body[w.offset:]
	w.offset = len(body)
}

func sqliteStreamFrame(t *testing.T, frames <-chan string) publicListFixture {
	t.Helper()
	select {
	case frame := <-frames:
		if !strings.HasPrefix(frame, "event: nodes\ndata: ") || !strings.HasSuffix(frame, "\n\n") {
			t.Fatalf("invalid nodes frame: %q", frame)
		}
		var page publicListFixture
		if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(frame, "event: nodes\ndata: "), "\n\n")), &page); err != nil {
			t.Fatal(err)
		}
		return page
	case <-time.After(4 * time.Second):
		t.Fatal("no change frame within the coalescing window")
		return publicListFixture{}
	}
}

func TestPublicStreamSQLiteAcceptedReportAndUnpublish(t *testing.T) {
	f := newAgentHTTP(t, 2)
	id, token := publishCacheNode(t, f)
	// Warm the exact GET cache entry consumed by the stream.
	publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	w := &sqliteStreamRecorder{ResponseRecorder: httptest.NewRecorder(), frames: make(chan string, 8)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream?limit=1", nil).WithContext(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("stream did not end after cancellation")
		}
	})
	if page := sqliteStreamFrame(t, w.frames); len(page.Nodes) != 1 {
		t.Fatalf("initial nodes: %+v", page)
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "stream-session", 25)), 1)
	if page := sqliteStreamFrame(t, w.frames); len(page.Nodes) != 1 {
		t.Fatalf("accepted report nodes: %+v", page)
	}
	// The fixed fixture clock keeps the warmed cache younger than its TTL.
	// Only committed visibility invalidation can remove the node here.
	enabled := false
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if page := sqliteStreamFrame(t, w.frames); len(page.Nodes) != 0 {
		t.Fatalf("unpublished node leaked through cached frame: %+v", page)
	}
}
