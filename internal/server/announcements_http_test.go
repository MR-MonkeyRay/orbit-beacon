package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/timekey"
	"strings"
	"testing"
	"time"
)

func TestAnnouncementHTTPCRUDAndGate(t *testing.T) {
	f := newHTTP(t, 1)
	admin, csrf := login(t, f, "admin-two")
	const collection = "/api/v1/announcements"
	body := `{"level":"warning","title":"Maintenance","body":"first\r\nsecond","ends_at":"2027-01-01T00:00:00Z"}`
	assertHTTPStatus(t, request(t, f.handler, "GET", collection, "", nil, "", ""), 401)
	assertHTTPStatus(t, request(t, f.handler, "POST", collection, body, nil, "", publicURL), 401)
	assertHTTPStatus(t, request(t, f.handler, "POST", collection, body, admin, "", publicURL), 403)
	r := request(t, f.handler, "POST", collection, body, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 201)
	id := objectID(t, decodeObject(t, r))
	path := collection + "/" + id
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"title":"changed"}`, admin, csrf, publicURL), 200)
	r = request(t, f.handler, "PATCH", path, `{"ends_at":null}`, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 200)
	if obj := decodeObject(t, r); obj["ends_at"] != nil || obj["body"] != "first\nsecond" {
		t.Fatal(obj)
	}
	for _, body := range []string{`{"title":null}`, `{"body":null}`, `{"starts_at":null}`, `{"level":null}`, `{"unknown":true}`} {
		assertHTTPStatus(t, request(t, f.handler, "PATCH", path, body, admin, csrf, publicURL), 400)
	}
	collectionContains(t, request(t, f.handler, "GET", collection, "", admin, "", ""), "announcements", id)
	r = request(t, f.handler, "GET", "/api/public/v1/nodes", "", nil, "", "")
	assertHTTPStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), "changed") || strings.Contains(r.Body.String(), id) {
		t.Fatal(r.Body)
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/settings/public-page", `{"enabled":false}`, admin, csrf, publicURL), 200)
	for _, p := range []string{"/api/public/v1/nodes", "/api/public/v1/stream", "/api/public/v1/ws"} {
		r = request(t, f.handler, "GET", p, "", nil, "", "")
		assertHTTPStatus(t, r, 403)
		if strings.Contains(r.Body.String(), "changed") {
			t.Fatal("disabled leak")
		}
	}
	assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", admin, csrf, publicURL), 204)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", admin, csrf, publicURL), 404)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{}`, admin, csrf, publicURL), 404)
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE target_id=? AND action LIKE 'announcement.%'", id).Scan(&count); err != nil || count != 4 {
		t.Fatalf("audit %d %v", count, err)
	}
}
func TestAnnouncementRetentionRealWiring(t *testing.T) {
	f := newHTTP(t, 1)
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	start := timekey.Format(now.Add(-200 * 24 * time.Hour))
	old := timekey.Format(now.Add(-91 * 24 * time.Hour))
	boundary := timekey.Format(now.Add(-90 * 24 * time.Hour))
	for i := 0; i < 1003; i++ {
		var end any = old
		if i == 1001 {
			end = nil
		}
		if i == 1002 {
			end = boundary
		}
		if _, err = f.db.Exec(`INSERT INTO announcements VALUES(?,'info','T','B',?,?,?,?)`, fmt.Sprint(i), start, end, start, start); err != nil {
			t.Fatal(err)
		}
	}
	result, err := h.CleanupRetention(context.Background(), now)
	if err != nil || result.Announcements != 1001 {
		t.Fatalf("retention %+v %v", result, err)
	}
	var count int
	f.db.QueryRow("SELECT count(*) FROM announcements").Scan(&count)
	if count != 2 {
		t.Fatal(count)
	}
}
func TestAnnouncementHTTPCapacity(t *testing.T) {
	f := newHTTP(t, 1)
	admin, csrf := login(t, f, "admin-two")
	for i := 0; i < 100; i++ {
		if _, err := f.db.Exec(`INSERT INTO announcements VALUES(?,'info','T','B',?,NULL,?,?)`, fmt.Sprint(i), "2026-01-01T00:00:00.000000000Z", "2026-01-01T00:00:00.000000000Z", "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatal(err)
		}
	}
	r := request(t, f.handler, "POST", "/api/v1/announcements", `{"level":"info","title":"T","body":"B"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 409)
	var e map[string]any
	json.Unmarshal(r.Body.Bytes(), &e)
	if !strings.Contains(r.Body.String(), "announcement_capacity_reached") {
		t.Fatal(e)
	}
}
