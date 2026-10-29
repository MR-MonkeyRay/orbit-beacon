package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/inbox"
)

const inboxAlertID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
const inboxTaskID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"

func insertHTTPInbox(t *testing.T, f fixture, id, kind string, at time.Time) {
	t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = inbox.Insert(context.Background(), tx, inbox.Event{ID: id, Kind: kind, SourceID: id, Transition: "succeeded", Severity: "info", CreatedAt: at, Details: json.RawMessage(`{"name":"Retained source snapshot","state":"succeeded"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func inboxHTTPSummary(t *testing.T, f fixture, cookie *http.Cookie) inbox.Summary {
	t.Helper()
	r := request(t, f.handler, "GET", "/api/v1/notifications/summary", "", cookie, "", "")
	assertHTTPStatus(t, r, 200)
	var sum inbox.Summary
	if err := json.Unmarshal(r.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"sequence", "password", "ciphertext", "credentials"} {
		if strings.Contains(r.Body.String(), forbidden) {
			t.Fatalf("summary exposed %s: %s", forbidden, r.Body)
		}
	}
	return sum
}

func TestInboxHTTPVisibilityReadIsolationAndSnapshots(t *testing.T) {
	f := newHTTP(t, 1)
	at := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	insertHTTPInbox(t, f, inboxAlertID, "alert", at)
	insertHTTPInbox(t, f, inboxTaskID, "task", at)
	admin, _ := login(t, f, "admin")
	second, csrf := login(t, f, "admin-two")
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/notifications", "", nil, "", ""), 401)
	if sum := inboxHTTPSummary(t, f, admin); sum.UnreadCount != 2 || sum.LatestID == nil || *sum.LatestID != inboxTaskID {
		t.Fatalf("admin summary=%+v", sum)
	}
	if sum := inboxHTTPSummary(t, f, second); sum.UnreadCount != 2 || sum.LatestID == nil || *sum.LatestID != inboxTaskID {
		t.Fatalf("second administrator summary=%+v", sum)
	}
	r := request(t, f.handler, "GET", "/api/v1/notifications", "", second, "", "")
	assertHTTPStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), inboxTaskID) || !strings.Contains(r.Body.String(), inboxAlertID) {
		t.Fatalf("visibility=%s", r.Body)
	}
	for _, forbidden := range []string{"sequence", "password", "ciphertext", "credentials"} {
		if strings.Contains(r.Body.String(), forbidden) {
			t.Fatalf("list exposed %s", forbidden)
		}
	}
	missing := request(t, f.handler, "GET", "/api/v1/notifications/00000000-0000-4000-8000-000000000099", "", second, "", "")
	assertHTTPStatus(t, missing, 404)
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/notifications/"+inboxTaskID, "", second, "", ""), 200)
	r = request(t, f.handler, "GET", "/api/v1/notifications/"+inboxAlertID, "", second, "", "")
	assertHTTPStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), "Retained source snapshot") {
		t.Fatal("source-less snapshot unavailable")
	}
	if sum := inboxHTTPSummary(t, f, second); sum.UnreadCount != 2 {
		t.Fatal("GET marked notification read")
	}
	path := "/api/v1/notifications/" + inboxAlertID + "/read"
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, second, "wrong", publicURL), 403)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, second, csrf, "https://foreign.test"), 403)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, second, csrf, publicURL), 204)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, second, csrf, publicURL), 204)
	if sum := inboxHTTPSummary(t, f, second); sum.UnreadCount != 1 {
		t.Fatal("own read not persisted")
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/notifications/"+inboxTaskID+"/read", `{"read":true}`, second, csrf, publicURL), 204)
	if sum := inboxHTTPSummary(t, f, second); sum.UnreadCount != 0 {
		t.Fatal("task read not persisted")
	}
	if sum := inboxHTTPSummary(t, f, admin); sum.UnreadCount != 2 {
		t.Fatal("read changed another user")
	}
	if _, err := f.auth.CreateUser(context.Background(), "admin-three", password, false); err != nil {
		t.Fatal(err)
	}
	third, _ := login(t, f, "admin-three")
	if sum := inboxHTTPSummary(t, f, third); sum.UnreadCount != 2 {
		t.Fatal("new user cannot see retained shared history")
	}
}

func TestInboxHTTPReadAllUsesInsertionWatermark(t *testing.T) {
	f := newHTTP(t, 1)
	at := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	insertHTTPInbox(t, f, inboxAlertID, "alert", at)
	insertHTTPInbox(t, f, inboxTaskID, "task", at)
	second, csrf := login(t, f, "admin-two")
	admin, _ := login(t, f, "admin")
	r := request(t, f.handler, "GET", "/api/v1/notifications?limit=1&unread=true", "", admin, "", "")
	assertHTTPStatus(t, r, 200)
	var page inbox.Page
	if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || len(page.Notifications) != 1 || page.Notifications[0].ID != inboxAlertID || page.ReadThrough == nil || *page.ReadThrough != inboxTaskID {
		t.Fatalf("watermark constrained to page: %+v", page)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", `{"through":"00000000-0000-4000-8000-000000000099"}`, second, csrf, publicURL), 404)
	if sum := inboxHTTPSummary(t, f, second); sum.UnreadCount != 2 {
		t.Fatal("missing watermark mutated reads")
	}
	for i, when := range []time.Time{at, at.Add(-time.Hour)} {
		id := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}[i]
		insertHTTPInbox(t, f, id, "alert", when)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", `{"through":"`+inboxTaskID+`"}`, second, csrf, publicURL), 204)
	if sum := inboxHTTPSummary(t, f, second); sum.UnreadCount != 2 {
		t.Fatalf("new notifications wrongly read: %+v", sum)
	}
	if _, err := f.db.Exec("DELETE FROM inbox_notifications WHERE id=?", inboxTaskID); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", `{"through":"`+inboxTaskID+`"}`, second, csrf, publicURL), 404)
	if sum := inboxHTTPSummary(t, f, second); sum.UnreadCount != 2 {
		t.Fatal("deleted watermark mutated reads")
	}
}

func TestInboxHTTPStrictQueriesBodiesAndRouting(t *testing.T) {
	f := newHTTP(t, 1)
	insertHTTPInbox(t, f, inboxAlertID, "alert", time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC))
	second, csrf := login(t, f, "admin-two")
	for _, query := range []string{"limit=0", "limit=101", "limit=x", "limit=", "limit=1&limit=2", "before=", "before=bad", "unread=", "unread=1", "unread=True", "unread=true&unread=false", "user_id=other", "%zz=x"} {
		assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/notifications?"+query, "", second, "", ""), 400)
	}
	for _, query := range []string{"limit=1", "limit=100&unread=false", "unread=true"} {
		assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/notifications?"+query, "", second, "", ""), 200)
	}
	for _, path := range []string{"/api/v1/notifications/summary", "/api/v1/notifications/" + inboxAlertID} {
		assertHTTPStatus(t, request(t, f.handler, "GET", path+"?limit=1", "", second, "", ""), 400)
	}
	for _, body := range []string{`{}`, `{"read":false}`, `{"read":null}`, `{"read":true,"user_id":"other"}`, `{"read":"true"}`} {
		assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/notifications/"+inboxAlertID+"/read", body, second, csrf, publicURL), 400)
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/notifications/"+inboxAlertID+"/read?x=1", `{"read":true}`, second, csrf, publicURL), 400)
	for _, body := range []string{`{}`, `{"through":null}`, `{"through":"bad"}`, `{"through":"` + inboxAlertID + `","user_id":"other"}`} {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", body, second, csrf, publicURL), 400)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all?x=1", `{"through":"`+inboxAlertID+`"}`, second, csrf, publicURL), 400)
	for _, tc := range []struct{ method, path, allow string }{
		{"POST", "/api/v1/notifications", "GET"},
		{"POST", "/api/v1/notifications/summary", "GET"},
		{"POST", "/api/v1/notifications/" + inboxAlertID, "GET"},
		{"GET", "/api/v1/notifications/" + inboxAlertID + "/read", "PATCH"},
		{"GET", "/api/v1/notifications/read-all", "POST"},
	} {
		r := request(t, f.handler, tc.method, tc.path, "", nil, "", "")
		assertHTTPStatus(t, r, 405)
		if r.Header().Get("Allow") != tc.allow {
			t.Fatalf("Allow=%q", r.Header().Get("Allow"))
		}
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/notifications/not-a-uuid/read", `{"read":true}`, second, csrf, publicURL), 404)
}
