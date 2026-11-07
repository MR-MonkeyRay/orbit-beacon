package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/nodes"
)

func TestPublicStreamSQLiteOrdinaryChangesStayPaced(t *testing.T) {
	for _, publishing := range []bool{false, true} {
		name := "public text edit"
		if publishing {
			name = "publication addition"
		}
		t.Run(name, func(t *testing.T) {
			f := newAgentHTTP(t, 2)
			id, _ := publishCacheNode(t, f)
			settings, err := f.nodes.GetPublicSettings(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			wantName := settings.Name
			patch := nodes.PublicSettingsPatch{}
			if publishing {
				disabled, enabled := false, true
				if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Enabled: &disabled}); err != nil {
					t.Fatal(err)
				}
				patch.Enabled = &enabled
			} else {
				wantName = "Updated public label"
				patch.Name = &wantName
			}
			base, started := f.now, time.Now()
			f.handler, err = server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: func() time.Time { return base.Add(time.Since(started)) }, WebUpdateIntervalSeconds: 2}))
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
					t.Error("ordinary update stream did not stop")
				}
			}()
			if kind, _, initial := sqliteStreamFrame(t, w.frames); kind != "snapshot" || (len(initial.Nodes) == 0) != publishing {
				t.Fatalf("unexpected initial inventory: type=%s page=%+v", kind, initial)
			}
			firstAt := time.Now()
			if _, err := f.nodes.PatchPublicSettings(context.Background(), id, patch); err != nil {
				t.Fatal(err)
			}
			kind, code, page := sqliteStreamFrame(t, w.frames)
			if kind != "snapshot" || code != "" || len(page.Nodes) != 1 {
				t.Fatalf("ordinary change renewed instead of a paced snapshot: type=%s code=%s page=%+v", kind, code, page)
			}
			var gotName string
			if err := json.Unmarshal(page.Nodes[0]["name"], &gotName); err != nil {
				t.Fatal(err)
			}
			if gotName != wantName {
				t.Fatalf("public name=%q want=%q", gotName, wantName)
			}
			if elapsed := time.Since(firstAt); elapsed < 1800*time.Millisecond {
				t.Fatalf("ordinary mutation bypassed two-second pacing: %v", elapsed)
			}
		})
	}
}
