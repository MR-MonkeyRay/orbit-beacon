package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/server"
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

func sqliteStreamFrame(t *testing.T, frames <-chan string) (string, string, publicListFixture) {
	t.Helper()
	select {
	case text := <-frames:
		var data string
		for _, line := range strings.Split(text, "\n") {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data += strings.TrimSpace(value)
			}
		}
		var frame struct {
			Type string            `json:"type"`
			Code string            `json:"code"`
			Data publicListFixture `json:"data"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatal(err)
		}
		return frame.Type, frame.Code, frame.Data
	case <-time.After(4 * time.Second):
		t.Fatal("no frame within the configured update interval")
		return "", "", publicListFixture{}
	}
}

func TestPublicStreamSQLiteAcceptedReportAndUnpublish(t *testing.T) {
	f := newAgentHTTP(t, 2)
	id, token := publishCacheNode(t, f)
	base, started := f.now, time.Now()
	var err error
	f.handler, err = server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: func() time.Time { return base.Add(time.Since(started)) }, WebUpdateIntervalSeconds: 2})
	if err != nil {
		t.Fatal(err)
	}
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
	if kind, _, page := sqliteStreamFrame(t, w.frames); kind != "snapshot" || len(page.Nodes) != 1 {
		t.Fatalf("initial nodes: %+v", page)
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "stream-session", 25)), 1)
	if kind, _, page := sqliteStreamFrame(t, w.frames); kind != "snapshot" || len(page.Nodes) != 1 || string(page.Nodes[0]["cpu_percent"]) != "25" {
		t.Fatalf("accepted report nodes: %+v", page)
	}
	// Withdrawal is a safety exception: close immediately, without waiting
	// for the ordinary report interval or emitting another page.
	enabled := false
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if kind, code, page := sqliteStreamFrame(t, w.frames); kind != "close" || code != "renew" || len(page.Nodes) != 0 {
		t.Fatalf("unpublish did not terminate the stream safely: type=%s code=%s page=%+v", kind, code, page)
	}
}

func TestPublicStreamReprojectsSourceAndPresenceExpiryWithoutNewReports(t *testing.T) {
	f := newAgentHTTP(t, 2)
	_, token := publishCacheNode(t, f)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "temporal-stream", 25)), 1)
	base := f.now
	var elapsedMS atomic.Int64
	var err error
	f.handler, err = server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: func() time.Time { return base.Add(time.Duration(elapsedMS.Load()) * time.Millisecond) }, WebUpdateIntervalSeconds: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &sqliteStreamRecorder{ResponseRecorder: httptest.NewRecorder(), frames: make(chan string, 8)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/stream", nil).WithContext(ctx))
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("temporal stream did not exit")
		}
	}()
	if kind, _, page := sqliteStreamFrame(t, w.frames); kind != "snapshot" || len(page.Nodes) != 1 || string(page.Nodes[0]["cpu_percent"]) != "25" {
		t.Fatalf("initial page=%+v", page)
	}
	for _, step := range []struct {
		elapsed  int64
		presence string
	}{{45001, `"online"`}, {180001, `"offline"`}} {
		elapsedMS.Store(step.elapsed)
		kind, _, page := sqliteStreamFrame(t, w.frames)
		if kind != "snapshot" || len(page.Nodes) != 1 {
			t.Fatalf("temporal projection=%+v", page)
		}
		node := page.Nodes[0]
		if string(node["status"]) != step.presence || string(node["cpu_percent"]) != "25" || string(node["uptime_seconds"]) != "100" {
			t.Fatalf("time-only transition cleared or advanced samples: %s", node)
		}
		publicMetricStatuses(t, node, "stale")
	}
}
