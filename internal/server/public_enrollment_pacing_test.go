package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/nodes"
)

func TestPublicStreamSQLiteFirstEnrollmentIsPacedAddition(t *testing.T) {
	f := newAgentHTTP(t, 2)
	pending, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "internal pending host"})
	if err != nil {
		t.Fatal(err)
	}
	enabled, label := true, "Prepublished node"
	if _, err := f.nodes.PatchPublicSettings(context.Background(), pending.ID, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &label}); err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	base, started := f.now, time.Now()
	f.handler, err = server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: func() time.Time { return base.Add(time.Since(started)) }, WebUpdateIntervalSeconds: 2})
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
			t.Error("enrollment stream did not exit")
		}
	}()
	if kind, _, page := sqliteStreamFrame(t, w.frames); kind != "snapshot" || len(page.Nodes) != 0 {
		t.Fatalf("pending identity was public: type=%s page=%+v", kind, page)
	}
	firstAt := time.Now()
	if _, err := f.enrollments.Enroll(context.Background(), ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "installation-first-public", NodeToken: agentSecret(155), System: v1.System{Hostname: "internal pending host", OS: "linux", Arch: "amd64"}}); err != nil {
		t.Fatal(err)
	}
	kind, code, page := sqliteStreamFrame(t, w.frames)
	if kind != "snapshot" || code != "" || len(page.Nodes) != 1 {
		t.Fatalf("first enrollment incorrectly renewed a healthy stream: type=%s code=%s page=%+v", kind, code, page)
	}
	var got string
	if err := json.Unmarshal(page.Nodes[0]["name"], &got); err != nil {
		t.Fatal(err)
	}
	if got != label {
		t.Fatalf("published name=%q want=%q", got, label)
	}
	if elapsed := time.Since(firstAt); elapsed < 1800*time.Millisecond {
		t.Fatalf("first enrollment bypassed configured pacing: %v", elapsed)
	}
}
