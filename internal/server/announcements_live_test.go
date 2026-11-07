package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"orbitbeacon/internal/server/announcements"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
	"strings"
	"testing"
	"time"
)

func TestPublicPagesEqualComparesAnnouncements(t *testing.T) {
	now := time.Now().UTC()
	a := ingest.PublicPage{Announcements: []announcements.Public{{Level: "info", Title: "T", Body: "B", StartsAt: now}}}
	b := a
	b.Announcements = append([]announcements.Public{}, a.Announcements...)
	if !publicPagesEqual(a, b) {
		t.Fatal("identical announcements differ")
	}
	for _, change := range []func(*announcements.Public){func(p *announcements.Public) { p.Level = "critical" }, func(p *announcements.Public) { p.Title = "other" }, func(p *announcements.Public) { p.Body = "other" }, func(p *announcements.Public) { p.StartsAt = now.Add(time.Second) }, func(p *announcements.Public) { end := now.Add(time.Hour); p.EndsAt = &end }} {
		b.Announcements[0] = a.Announcements[0]
		change(&b.Announcements[0])
		if publicPagesEqual(a, b) {
			t.Fatal("change missed")
		}
	}
	b.Announcements = nil
	if publicPagesEqual(a, b) {
		t.Fatal("removal missed")
	}
}

func TestAnnouncementServiceImmediatelyRenewsActivePublicSSE(t *testing.T) {
	_, stable := testMainDatabases(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ns := nodes.NewService(stable, nodes.Options{Now: clock})
	h := &handler{nodes: ns, ingest: ingest.NewService(stable, ingest.Options{Now: clock}), now: clock, metrics: newHTTPMetrics("test"), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), streamInterval: time.Hour, streamHeartbeat: time.Hour, streamLifetime: time.Hour}
	svc := newAnnouncementService(ns, clock)
	w := newStreamRecorder()
	cancel, done := startPublicStream(t, h, w)
	defer func() { cancel(); waitStreamDone(t, done) }()
	first := nextFlush(t, w)
	if !strings.Contains(first, `"announcements":[]`) {
		t.Fatal(first)
	}
	var patch announcements.Patch
	json.Unmarshal([]byte(`{"level":"critical","title":"Maintenance","body":"plain text"}`), &patch)
	a, err := svc.Save(context.Background(), "", patch)
	if err != nil {
		t.Fatal(err)
	}
	// 安全通知立即唤醒空闲流，即使更新间隔为一小时。
	frame := lastLiveTestFrame(t, nextFlush(t, w))
	if string(frame["type"]) != `"close"` || string(frame["code"]) != `"renew"` {
		t.Fatalf("%s", w.Body())
	}
	waitStreamDone(t, done)
	r := httptest.NewRecorder()
	h.publicNodes(r, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil), "test")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Maintenance") || strings.Contains(r.Body.String(), a.ID) {
		t.Fatalf("renewed snapshot %d %s", r.Code, r.Body)
	}
	h.publicPageDisabled.Store(true)
	for _, path := range []string{"/api/public/v1/nodes", "/api/public/v1/stream", "/api/public/v1/ws"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 403 || strings.Contains(w.Body.String(), "Maintenance") {
			t.Fatalf("disabled %s %d %s", path, w.Code, w.Body)
		}
	}
}
func TestAnnouncementCleanupRetentionRealSQLiteConnection(t *testing.T) {
	db, stable := testMainDatabases(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	h := &handler{nodes: nodes.NewService(stable, nodes.Options{}), management: management.NewService(stable), enrollments: enrollment.NewService(stable, enrollment.Options{Now: clock}), auth: auth.NewService(stable, auth.Options{Now: clock}), announcements: announcements.New(stable, clock, nil)}
	start := timekey.Format(now.Add(-200 * 24 * time.Hour))
	old := timekey.Format(now.Add(-91 * 24 * time.Hour))
	for i := 0; i < 1003; i++ {
		var end any = old
		if i == 1001 {
			end = nil
		}
		if i == 1002 {
			end = timekey.Format(now.Add(-90 * 24 * time.Hour))
		}
		if _, err := db.Exec(`INSERT INTO announcements VALUES(?,'info','T','B',?,?,?,?)`, fmt.Sprint(i), start, end, start, start); err != nil {
			t.Fatal(err)
		}
	}
	result, err := h.CleanupRetention(context.Background(), now)
	if err != nil || result.Announcements != 1001 {
		t.Fatalf("cleanup %+v %v", result, err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM announcements").Scan(&count); err != nil || count != 2 {
		t.Fatalf("retained %d %v", count, err)
	}
	if !retentionChanged(result) {
		t.Fatal("retention result not logged")
	}
}

func TestAnnouncementHTTPHandlerPreservesPatchID(t *testing.T) {
	_, stable := testMainDatabases(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	h := &handler{announcements: announcements.New(stable, func() time.Time { return now }, nil)}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		h.announcementsRoute(w, r, "test")
		return w
	}
	created := call(http.MethodPost, "/api/v1/announcements", `{"level":"warning","title":"Original","body":"Body","ends_at":"2027-01-01T00:00:00Z"}`)
	if created.Code != 201 {
		t.Fatalf("create %d %s", created.Code, created.Body)
	}
	var a announcements.Announcement
	if err := json.Unmarshal(created.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/announcements/" + a.ID
	changed := call(http.MethodPatch, path, `{"title":"Changed","ends_at":null}`)
	if changed.Code != 200 {
		t.Fatalf("patch %d %s", changed.Code, changed.Body)
	}
	var updated announcements.Announcement
	json.Unmarshal(changed.Body.Bytes(), &updated)
	if updated.ID != a.ID || updated.Title != "Changed" || updated.Body != "Body" || updated.EndsAt != nil {
		t.Fatalf("patch %+v", updated)
	}
	if w := call(http.MethodDelete, path, ""); w.Code != 204 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if w := call(http.MethodDelete, path, ""); w.Code != 404 {
		t.Fatalf("missing delete %d %s", w.Code, w.Body)
	}
}
