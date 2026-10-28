package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
)

const validChannel = `{"name":"Primary webhook","kind":"webhook","enabled":true,"config":{"url":"https://hooks.example.com/notify"},"secret":"NEVER-EXPOSE-CHANNEL-SECRET"}`

func TestNotificationHTTPCRUDRoutingAuthzAndSecretSafety(t *testing.T) {
	f := newHTTP(t, 2)
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	notifications := notify.New(f.db, box, notify.Options{})
	var logs bytes.Buffer
	h, e := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Notifications: notifications, PublicURL: publicURL, AccessLog: true, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if e != nil {
		t.Fatal(e)
	}
	f.handler = h
	admin, csrf := login(t, f, "admin")
	viewer, vcsrf := login(t, f, "viewer")
	r := request(t, h, "POST", "/api/v1/notification-channels", validChannel, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 201)
	obj := decodeObject(t, r)
	id := objectID(t, obj)
	path := "/api/v1/notification-channels/" + id
	if obj["secret_set"] != true || strings.Contains(r.Body.String(), "NEVER-EXPOSE") {
		t.Fatal("secret projection incorrect")
	}
	collectionContains(t, request(t, h, "GET", "/api/v1/notification-channels", "", viewer, "", ""), "channels", id)
	assertHTTPStatus(t, request(t, h, "GET", path, "", viewer, "", ""), 200)
	for _, tt := range []struct{ method, path, body string }{{"POST", "/api/v1/notification-channels", validChannel}, {"PATCH", path, `{"name":"changed"}`}, {"DELETE", path, ""}, {"POST", path + "/test", ""}} {
		assertHTTPStatus(t, request(t, h, tt.method, tt.path, tt.body, viewer, vcsrf, publicURL), 403)
		assertHTTPStatus(t, request(t, h, tt.method, tt.path, tt.body, admin, "", publicURL), 403)
		assertHTTPStatus(t, request(t, h, tt.method, tt.path, tt.body, admin, csrf, "https://foreign.test"), 403)
	}
	for _, body := range []string{`{}`, `{"name":null}`, `{"name":""}`, `{"name":"bad","kind":"unknown","enabled":true,"config":{}}`, strings.Replace(validChannel, "https://hooks.example.com/notify", "http://127.0.0.1", 1), strings.Replace(validChannel, `"config":{"url":"https://hooks.example.com/notify"}`, `"config":{"url":"https://u:p@example.com"}`, 1), strings.Replace(validChannel, `"kind":"webhook"`, `"kind":"telegram"`, 1), strings.Replace(validChannel, `"config":{"url":"https://hooks.example.com/notify"}`, `"config":{"url":"https://hooks.example.com","secret":"bad"}`, 1)} {
		assertHTTPStatus(t, request(t, h, "POST", "/api/v1/notification-channels", body, admin, csrf, publicURL), 400)
	}
	ruleBody := strings.TrimSuffix(validAlertRule, "}") + `,"channel_ids":["` + id + `"]}`
	r = request(t, h, "POST", "/api/v1/alert-rules", ruleBody, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 201)
	ruleID := objectID(t, decodeObject(t, r))
	rp := "/api/v1/alert-rules/" + ruleID
	for _, body := range []string{`{"channel_ids":null}`, `{"channel_ids":["00000000-0000-4000-8000-000000000099"]}`, `{"channel_ids":["` + id + `","` + id + `"]}`} {
		assertHTTPStatus(t, request(t, h, "PATCH", rp, body, admin, csrf, publicURL), 400)
	}
	r = request(t, h, "PATCH", rp, `{"name":"kept routes"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 200)
	if !strings.Contains(r.Body.String(), id) {
		t.Fatal("routing dropped on omitted channel_ids")
	}
	assertHTTPStatus(t, request(t, h, "GET", "/api/v1/notification-deliveries", "", viewer, "", ""), 200)
	for _, query := range []string{"limit=0", "limit=101", "limit=x", "limit=", "limit=2&limit=3", "before=bad", "before=", "offset=0", "%zz=x"} {
		assertHTTPStatus(t, request(t, h, "GET", "/api/v1/notification-deliveries?"+query, "", viewer, "", ""), 400)
	}
	// A test against a private address is denied before network I/O and audited.
	assertHTTPStatus(t, request(t, h, "PATCH", path, `{"config":{"url":"https://127.0.0.1"}}`, admin, csrf, publicURL), 200)
	r = request(t, h, "POST", path+"/test", "", admin, csrf, publicURL)
	assertHTTPStatus(t, r, 200)
	var result notify.Result
	if json.Unmarshal(r.Body.Bytes(), &result) != nil || result.OK || result.ErrorCode != "destination_denied" {
		t.Fatalf("test=%s", r.Body)
	}
	assertHTTPStatus(t, request(t, h, "PATCH", path, `{"secret":""}`, admin, csrf, publicURL), 200)
	assertHTTPStatus(t, request(t, h, "DELETE", path, "", admin, csrf, publicURL), 204)
	assertHTTPStatus(t, request(t, h, "GET", path, "", viewer, "", ""), 404)
	var routes int
	f.db.QueryRow("SELECT count(*) FROM alert_rule_channels WHERE rule_id=?", ruleID).Scan(&routes)
	if routes != 0 {
		t.Fatal("channel delete did not cascade routing")
	}
	rows, e := f.db.Query("SELECT details FROM audit_events WHERE action LIKE 'notification_channel.%'")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var raw string
		rows.Scan(&raw)
		count++
		if strings.Contains(raw, "NEVER-EXPOSE") {
			t.Fatal("secret in audit")
		}
	}
	if count != 5 {
		t.Fatalf("channel audit count=%d", count)
	}
	if strings.Contains(logs.String(), "NEVER-EXPOSE") || strings.Contains(logs.String(), "hooks.example.com") {
		t.Fatal("secret or config in logs")
	}
}
func TestNotificationHTTP405AndCapacity(t *testing.T) {
	f := newHTTP(t, 1)
	box, _ := secretbox.New(bytes.Repeat([]byte{1}, 32))
	ns := notify.New(f.db, box, notify.Options{})
	h, e := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Notifications: ns, PublicURL: publicURL})
	if e != nil {
		t.Fatal(e)
	}
	f.handler = h
	for _, tt := range []struct{ path, method, allow string }{{"/api/v1/notification-channels", "PUT", "GET, POST"}, {"/api/v1/notification-channels/00000000-0000-4000-8000-000000000001", "POST", "GET, PATCH, DELETE"}, {"/api/v1/notification-channels/00000000-0000-4000-8000-000000000001/test", "GET", "POST"}, {"/api/v1/notification-deliveries", "POST", "GET"}} {
		r := request(t, h, tt.method, tt.path, "", nil, "", "")
		assertHTTPStatus(t, r, 405)
		if r.Header().Get("Allow") != tt.allow {
			t.Fatal("bad Allow")
		}
	}
	admin, csrf := login(t, f, "admin")
	for range 20 {
		assertHTTPStatus(t, request(t, h, "POST", "/api/v1/notification-channels", validChannel, admin, csrf, publicURL), 201)
	}
	assertHTTPStatus(t, request(t, h, "POST", "/api/v1/notification-channels", validChannel, admin, csrf, publicURL), 409)
	var p notify.Patch
	json.Unmarshal([]byte(`{"name":"changed"}`), &p)
	if _, e = ns.Save(context.Background(), "missing", p); e != notify.ErrNotFound {
		t.Fatal(e)
	}
}

func TestWebhookURLPrivacyAndKeyMismatchRepairHTTP(t *testing.T) {
	f := newHTTP(t, 1)
	oldBox, _ := secretbox.New(bytes.Repeat([]byte{3}, 32))
	ns := notify.New(f.db, oldBox, notify.Options{})
	var logs bytes.Buffer
	makeHandler := func(n *notify.Service) {
		t.Helper()
		h, e := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Notifications: n, PublicURL: publicURL, AccessLog: true, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
		if e != nil {
			t.Fatal(e)
		}
		f.handler = h
	}
	makeHandler(ns)
	admin, csrf := login(t, f, "admin")
	viewer, _ := login(t, f, "viewer")
	const pathMarker = "PATH-MUST-STAY-PRIVATE"
	const queryMarker = "QUERY-MUST-STAY-PRIVATE"
	body := `{"name":"Private webhook","kind":"webhook","enabled":true,"config":{"url":"https://hooks.example.com/` + pathMarker + `?token=` + queryMarker + `"},"secret":"OLD-SIGNING-SECRET"}`
	r := request(t, f.handler, "POST", "/api/v1/notification-channels", body, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 201)
	id := objectID(t, decodeObject(t, r))
	path := "/api/v1/notification-channels/" + id
	check := func(raw string) {
		t.Helper()
		for _, value := range []string{pathMarker, queryMarker, "OLD-SIGNING-SECRET", "NEW-SIGNING-SECRET"} {
			if strings.Contains(raw, value) {
				t.Fatal("secret material exposed")
			}
		}
	}
	check(r.Body.String())
	for _, p := range []string{path, "/api/v1/notification-channels"} {
		r = request(t, f.handler, "GET", p, "", viewer, "", "")
		assertHTTPStatus(t, r, 200)
		check(r.Body.String())
		if !strings.Contains(r.Body.String(), `"url_redacted":"https://hooks.example.com/…"`) || !strings.Contains(r.Body.String(), `"url_set":true`) || strings.Contains(r.Body.String(), `"url":`) {
			t.Fatal("unsafe URL projection")
		}
	}
	wrongBox, _ := secretbox.New(bytes.Repeat([]byte{4}, 32))
	makeHandler(notify.New(f.db, wrongBox, notify.Options{}))
	r = request(t, f.handler, "POST", path+"/test", "", admin, csrf, publicURL)
	assertHTTPStatus(t, r, 409)
	check(r.Body.String())
	if decodeObject(t, r)["code"] != "secret_unreadable" || !strings.Contains(r.Body.String(), "Restore") {
		t.Fatal("missing repair guidance")
	}
	for _, secret := range []string{`"NEW-SIGNING-SECRET"`, `""`} {
		r = request(t, f.handler, "PATCH", path, `{"secret":`+secret+`}`, admin, csrf, publicURL)
		assertHTTPStatus(t, r, 200)
		check(r.Body.String())
	}
	r = request(t, f.handler, "PATCH", path, `{"config":{}}`, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 200)
	check(r.Body.String())
	r = request(t, f.handler, "PATCH", path, `{"config":{"url":"https://hooks.example.com/REPLACEMENT-PATH?token=REPLACEMENT-QUERY"}}`, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 200)
	check(r.Body.String())
	if strings.Contains(r.Body.String(), "REPLACEMENT-") {
		t.Fatal("replacement URL exposed")
	}
	r = request(t, f.handler, "PATCH", path, `{"config":{"url":"https://user:password@hooks.example.com/secret"}}`, admin, csrf, publicURL)
	assertHTTPStatus(t, r, 400)
	rows, e := f.db.Query("SELECT details FROM audit_events WHERE action LIKE 'notification_channel.%'")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			t.Fatal(e)
		}
		check(raw)
		if strings.Contains(raw, "REPLACEMENT-") {
			t.Fatal("URL in audit")
		}
	}
	check(logs.String())
	if strings.Contains(logs.String(), "REPLACEMENT-") {
		t.Fatal("URL in logs")
	}
}
