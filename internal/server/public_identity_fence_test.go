package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server"
)

func TestPublicHTTPRevocationAfterProjectionDiscardsOldMetrics(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token := publishCacheNode(t, f)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "revoked-session", 25)), 1)
	before, raw := publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
	if len(before.Nodes) != 1 || string(before.Nodes[0]["cpu_percent"]) != "25" {
		t.Fatalf("initial public report: %s", raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projected, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	calls := 0
	clock := func() time.Time {
		calls++
		// 第一次取时间用于页面缓存，第二次用于已投影页面的响应帧。
		// 在序列化前让撤销完成，确保真正竞争的是带旧指标的页面。
		if calls == 2 {
			close(projected)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		}
		return f.now
	}
	var err error
	f.handler, err = server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: clock, WebUpdateIntervalSeconds: 2}))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes?limit=1", nil).WithContext(ctx))
	}()
	defer func() {
		release.Do(func() { close(resume) })
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("public request did not finish")
		}
	}()
	select {
	case <-projected:
	case <-ctx.Done():
		t.Fatal("public request did not reach serialization barrier")
	}
	if err := f.nodes.RevokeCredential(ctx, id); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(resume) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("public request did not resume after revocation")
	}
	if w.Code != http.StatusServiceUnavailable || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"snapshot_busy"`)) || bytes.Contains(w.Body.Bytes(), []byte(`"cpu_percent"`)) || bytes.Contains(w.Body.Bytes(), []byte("Public cache")) {
		t.Fatalf("revoked projection reached HTTP response: status=%d body=%s", w.Code, w.Body)
	}
	// 固定时钟并复用缓存键，撤销之后的新请求也不能拿回旧页面。
	after, raw := publicList(t, f, "/api/public/v1/nodes?limit=1", nil)
	if len(after.Nodes) != 0 || after.HasMore || after.NextOffset != nil {
		t.Fatalf("revoked projection remained cached: %s", raw)
	}
}
