package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/inbox"
)

const inboxAlertID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
const inboxTaskID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"

func insertHTTPInbox(t *testing.T, f fixture, id, kind, audience string, at time.Time) {
	t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = inbox.Insert(context.Background(), tx, inbox.Event{ID: id, Kind: kind, SourceID: id, Transition: "succeeded", Audience: audience, Severity: "info", CreatedAt: at, Details: json.RawMessage(`{"name":"Retained source snapshot","state":"succeeded"}`)})
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
	for _, forbidden := range []string{"sequence", "password", "ciphertext", "credentials", "audience"} {
		if strings.Contains(r.Body.String(), forbidden) {
			t.Fatalf("summary exposed %s: %s", forbidden, r.Body)
		}
	}
	return sum
}

func TestInboxHTTPVisibilityReadIsolationAndSnapshots(t *testing.T) {
	f := newHTTP(t, 1)
	at := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	insertHTTPInbox(t, f, inboxAlertID, "alert", "authenticated", at)
	insertHTTPInbox(t, f, inboxTaskID, "task", "admin", at)
	admin, _ := login(t, f, "admin")
	viewer, csrf := login(t, f, "viewer")
	assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/notifications", "", nil, "", ""), 401)
	if sum := inboxHTTPSummary(t, f, admin); sum.UnreadCount != 2 || sum.LatestID == nil || *sum.LatestID != inboxTaskID {
		t.Fatalf("admin summary=%+v", sum)
	}
	if sum := inboxHTTPSummary(t, f, viewer); sum.UnreadCount != 1 || sum.LatestID == nil || *sum.LatestID != inboxAlertID {
		t.Fatalf("viewer summary=%+v", sum)
	}
	r := request(t, f.handler, "GET", "/api/v1/notifications", "", viewer, "", "")
	assertHTTPStatus(t, r, 200)
	if strings.Contains(r.Body.String(), inboxTaskID) || !strings.Contains(r.Body.String(), inboxAlertID) {
		t.Fatalf("visibility=%s", r.Body)
	}
	for _, forbidden := range []string{"sequence", "password", "ciphertext", "credentials", "audience"} {
		if strings.Contains(r.Body.String(), forbidden) {
			t.Fatalf("list exposed %s", forbidden)
		}
	}
	missing := request(t, f.handler, "GET", "/api/v1/notifications/00000000-0000-4000-8000-000000000099", "", viewer, "", "")
	hidden := request(t, f.handler, "GET", "/api/v1/notifications/"+inboxTaskID, "", viewer, "", "")
	assertHTTPStatus(t, missing, 404)
	assertHTTPStatus(t, hidden, 404)
	if decodeObject(t, missing)["code"] != decodeObject(t, hidden)["code"] {
		t.Fatal("invisible notification distinguished from missing")
	}
	r = request(t, f.handler, "GET", "/api/v1/notifications/"+inboxAlertID, "", viewer, "", "")
	assertHTTPStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), "Retained source snapshot") {
		t.Fatal("source-less snapshot unavailable")
	}
	if sum := inboxHTTPSummary(t, f, viewer); sum.UnreadCount != 1 {
		t.Fatal("GET marked notification read")
	}
	path := "/api/v1/notifications/" + inboxAlertID + "/read"
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, viewer, "wrong", publicURL), 403)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, viewer, csrf, "https://foreign.test"), 403)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, viewer, csrf, publicURL), 204)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"read":true}`, viewer, csrf, publicURL), 204)
	if sum := inboxHTTPSummary(t, f, viewer); sum.UnreadCount != 0 {
		t.Fatal("own read not persisted")
	}
	if sum := inboxHTTPSummary(t, f, admin); sum.UnreadCount != 2 {
		t.Fatal("read changed another user")
	}
	if _, err := f.auth.CreateUser(context.Background(), "newviewer", password, auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	newviewer, _ := login(t, f, "newviewer")
	if sum := inboxHTTPSummary(t, f, newviewer); sum.UnreadCount != 1 {
		t.Fatal("new user cannot see retained shared history")
	}
}

func TestInboxHTTPReadAllUsesVisibleInsertionWatermark(t *testing.T) {
	f := newHTTP(t, 1)
	at := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	insertHTTPInbox(t, f, inboxAlertID, "alert", "authenticated", at)
	insertHTTPInbox(t, f, inboxTaskID, "task", "admin", at)
	viewer, csrf := login(t, f, "viewer")
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
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", `{"through":"`+inboxTaskID+`"}`, viewer, csrf, publicURL), 404)
	if sum := inboxHTTPSummary(t, f, viewer); sum.UnreadCount != 1 {
		t.Fatal("invisible watermark mutated reads")
	}
	for i, when := range []time.Time{at, at.Add(-time.Hour)} {
		id := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}[i]
		insertHTTPInbox(t, f, id, "alert", "authenticated", when)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", `{"through":"`+inboxAlertID+`"}`, viewer, csrf, publicURL), 204)
	if sum := inboxHTTPSummary(t, f, viewer); sum.UnreadCount != 2 {
		t.Fatalf("new notifications wrongly read: %+v", sum)
	}
	if _, err := f.db.Exec("DELETE FROM inbox_notifications WHERE id=?", inboxAlertID); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", `{"through":"`+inboxAlertID+`"}`, viewer, csrf, publicURL), 404)
	if sum := inboxHTTPSummary(t, f, viewer); sum.UnreadCount != 2 {
		t.Fatal("deleted watermark mutated reads")
	}
}

func TestInboxHTTPStrictQueriesBodiesAndRouting(t *testing.T) {
	f := newHTTP(t, 1)
	insertHTTPInbox(t, f, inboxAlertID, "alert", "authenticated", time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC))
	viewer, csrf := login(t, f, "viewer")
	for _, query := range []string{"limit=0", "limit=101", "limit=x", "limit=", "limit=1&limit=2", "before=", "before=bad", "unread=", "unread=1", "unread=True", "unread=true&unread=false", "user_id=other", "%zz=x"} {
		assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/notifications?"+query, "", viewer, "", ""), 400)
	}
	for _, query := range []string{"limit=1", "limit=100&unread=false", "unread=true"} {
		assertHTTPStatus(t, request(t, f.handler, "GET", "/api/v1/notifications?"+query, "", viewer, "", ""), 200)
	}
	for _, path := range []string{"/api/v1/notifications/summary", "/api/v1/notifications/" + inboxAlertID} {
		assertHTTPStatus(t, request(t, f.handler, "GET", path+"?limit=1", "", viewer, "", ""), 400)
	}
	for _, body := range []string{`{}`, `{"read":false}`, `{"read":null}`, `{"read":true,"user_id":"other"}`, `{"read":"true"}`} {
		assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/notifications/"+inboxAlertID+"/read", body, viewer, csrf, publicURL), 400)
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/notifications/"+inboxAlertID+"/read?x=1", `{"read":true}`, viewer, csrf, publicURL), 400)
	for _, body := range []string{`{}`, `{"through":null}`, `{"through":"bad"}`, `{"through":"` + inboxAlertID + `","user_id":"other"}`} {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all", body, viewer, csrf, publicURL), 400)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/notifications/read-all?x=1", `{"through":"`+inboxAlertID+`"}`, viewer, csrf, publicURL), 400)
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
	assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/notifications/not-a-uuid/read", `{"read":true}`, viewer, csrf, publicURL), 404)
}
