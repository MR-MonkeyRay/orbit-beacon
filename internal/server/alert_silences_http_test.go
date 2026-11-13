package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestSilenceHTTPValidationAuditAndRetention(t *testing.T) {
	f := newHTTP(t, 2)
	admin, csrf := login(t, f, "admin-two")
	now := time.Now().UTC()
	start := now.Add(time.Hour)
	end := start.Add(time.Hour)
	valid := map[string]any{"reason": " maintenance ", "scope_kind": "all", "scope_id": nil, "starts_at": start, "ends_at": end}
	encode := func(body map[string]any) string { raw, _ := json.Marshal(body); return string(raw) }
	for _, method := range []string{"GET", "POST", "DELETE"} {
		path := "/api/v1/alert-silences"
		if method == "DELETE" {
			path += "/00000000-0000-4000-8000-000000000099"
		}
		assertHTTPStatus(t, request(t, f.handler, method, path, encode(valid), nil, "", ""), 401)
	}
	for _, body := range []string{`{}`, `{"reason":null}`, `{"reason":1}`, `{"unknown":1}`, `{"reason":"r","scope_kind":"all"}`, `{"reason":"r","ends_at":"2030-01-01T00:00:00Z"}`} {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/alert-silences", body, admin, csrf, publicURL), 400)
	}
	cases := []struct {
		key   string
		value any
	}{
		{"reason", " "}, {"reason", strings.Repeat("界", 86)}, {"scope_kind", "bad"}, {"scope_kind", nil}, {"scope_kind", 1}, {"scope_id", "bad"}, {"scope_id", 1}, {"starts_at", nil}, {"starts_at", "bad"}, {"starts_at", false}, {"starts_at", time.Time{}}, {"ends_at", nil}, {"ends_at", false}, {"ends_at", "bad"}, {"ends_at", start}, {"ends_at", now.Add(-time.Hour)}, {"ends_at", start.Add(30*24*time.Hour + time.Second)},
	}
	for _, tc := range cases {
		body := map[string]any{}
		for k, v := range valid {
			body[k] = v
		}
		body[tc.key] = tc.value
		resp := request(t, f.handler, "POST", "/api/v1/alert-silences", encode(body), admin, csrf, publicURL)
		assertHTTPStatus(t, resp, 400)
		if !strings.Contains(resp.Body.String(), `"fields"`) {
			t.Fatalf("no field error: %s", resp.Body.String())
		}
	}
	for _, kind := range []string{"node", "group", "service_group"} {
		for _, scope := range []any{nil, "bad", "00000000-0000-4000-8000-000000000099"} {
			body := map[string]any{"reason": "scope", "scope_kind": kind, "scope_id": scope, "ends_at": end}
			response := request(t, f.handler, "POST", "/api/v1/alert-silences", encode(body), admin, csrf, publicURL)
			assertHTTPStatus(t, response, 400)
			if !strings.Contains(response.Body.String(), `"scope_id"`) {
				t.Fatalf("missing scope_id field error: %s", response.Body.String())
			}
		}
	}
	for _, body := range []map[string]any{
		{"reason": "expired", "scope_kind": "all", "starts_at": now.Add(-2 * time.Hour), "ends_at": now.Add(-time.Hour)},
		{"reason": "expired", "scope_kind": "all", "starts_at": now.Add(-time.Hour), "ends_at": now},
	} {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/alert-silences", encode(body), admin, csrf, publicURL), 400)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/alert-silences", encode(valid), admin, "", publicURL), 403)
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/alert-silences", encode(valid), admin, csrf, "https://foreign.test"), 403)
	resp := request(t, f.handler, "POST", "/api/v1/alert-silences", encode(valid), admin, csrf, publicURL)
	assertHTTPStatus(t, resp, 201)
	obj := decodeObject(t, resp)
	id := objectID(t, obj)
	if obj["reason"] != "maintenance" || obj["active"] != false || obj["scope_name"] != "" {
		t.Fatalf("silence=%+v", obj)
	}
	collectionContains(t, request(t, f.handler, "GET", "/api/v1/alert-silences", "", admin, "", ""), "silences", id)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/alert-silences/"+id, "", admin, "", publicURL), 403)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/alert-silences/"+id, "", admin, csrf, "https://foreign.test"), 403)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/alert-silences/"+id, "", admin, csrf, publicURL), 204)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/alert-silences/"+id, "", admin, csrf, publicURL), 404)
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE target_id=? AND action IN ('alert_silence.create','alert_silence.delete')", id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("audits=%d %v", count, err)
	}
	boundary := map[string]any{"reason": strings.Repeat("a", 255), "scope_kind": "all", "starts_at": start, "ends_at": start.Add(30 * 24 * time.Hour)}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/alert-silences", encode(boundary), admin, csrf, publicURL), 201)
	// Exercise the actual retention entry point, including the strict 90-day boundary.
	for i, at := range []time.Time{now.Add(-91 * 24 * time.Hour), now.Add(-90 * 24 * time.Hour), now.Add(time.Hour)} {
		_, err := f.db.Exec("INSERT INTO alert_silences(id,reason,scope_kind,starts_at,ends_at,created_at) VALUES(?,'old','all',?,?,?)", string(rune('a'+i)), timekey.Format(at.Add(-time.Hour)), timekey.Format(at), timekey.Format(at.Add(-time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
	}
	cleaner := f.handler.(interface {
		CleanupRetention(context.Context, time.Time) (server.RetentionResult, error)
	})
	result, err := cleaner.CleanupRetention(context.Background(), now)
	if err != nil || result.AlertSilences != 1 {
		t.Fatalf("cleanup=%+v %v", result, err)
	}
}
func TestSilenceHTTPDefaultStartScopeNamesAndCascade(t *testing.T) {
	for _, kind := range []string{"node", "group", "service_group"} {
		t.Run(kind, func(t *testing.T) {
			f := newHTTP(t, 2)
			admin, csrf := login(t, f, "admin")
			var id, path string
			if kind == "service_group" {
				id = createHTTPServiceGroup(t, f, admin, csrf, "scope")
				path = "/api/v1/service-groups/" + id
			} else if kind == "group" {
				id = createHTTPGroup(t, f, admin, csrf, "scope")
				path = "/api/v1/groups/" + id
			} else {
				node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				id = node.ID
				path = "/api/v1/nodes/" + id + "?confirm=true"
			}
			raw, _ := json.Marshal(map[string]any{"reason": "scope", "scope_kind": kind, "scope_id": id, "ends_at": time.Now().Add(time.Hour)})
			resp := request(t, f.handler, "POST", "/api/v1/alert-silences", string(raw), admin, csrf, publicURL)
			assertHTTPStatus(t, resp, 201)
			obj := decodeObject(t, resp)
			silenceID := objectID(t, obj)
			if obj["active"] != true || obj["scope_name"] != "scope" {
				t.Fatalf("silence=%+v", obj)
			}
			listed := request(t, f.handler, "GET", "/api/v1/alert-silences", "", admin, "", "")
			assertHTTPStatus(t, listed, 200)
			if !strings.Contains(listed.Body.String(), `"scope_name":"scope"`) {
				t.Fatal(listed.Body.String())
			}
			var audits int
			if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='alert_silence.create' AND target_id=?", silenceID).Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("audit=%d %v", audits, err)
			}
			assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", admin, csrf, publicURL), 204)
			var count int
			if err := f.db.QueryRow("SELECT count(*) FROM alert_silences WHERE id=?", silenceID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("cascade=%d %v", count, err)
			}
		})
	}
}

func TestSilenceHTTPListOrderingNamesAndAuditAtomicity(t *testing.T) {
	f := newHTTP(t, 2)
	admin, csrf := login(t, f, "admin")
	now := time.Now().UTC()
	group := createHTTPGroup(t, f, admin, csrf, "original")
	create := func(start time.Time, kind string, id *string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"reason": "maintenance", "scope_kind": kind, "scope_id": id, "starts_at": start, "ends_at": start.Add(time.Hour)})
		response := request(t, f.handler, "POST", "/api/v1/alert-silences", string(raw), admin, csrf, publicURL)
		assertHTTPStatus(t, response, 201)
		return objectID(t, decodeObject(t, response))
	}
	later := create(now.Add(2*time.Hour), "all", nil)
	active := create(now.Add(-time.Minute), "group", &group)
	earlier := create(now.Add(time.Hour), "all", nil)
	if _, err := f.db.Exec("UPDATE groups SET name='current' WHERE id=?", group); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO alert_silences(id,reason,scope_kind,starts_at,ends_at,created_at) VALUES('expired','old','all',?,?,?)`, timekey.Format(now.Add(-2*time.Hour)), timekey.Format(now.Add(-time.Hour)), timekey.Format(now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, "GET", "/api/v1/alert-silences", "", admin, "", "")
	assertHTTPStatus(t, response, 200)
	var object struct {
		Silences []map[string]any `json:"silences"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	if len(object.Silences) != 3 {
		t.Fatalf("list=%s", response.Body.String())
	}
	for i, id := range []string{active, earlier, later} {
		if object.Silences[i]["id"] != id {
			t.Fatalf("order=%s", response.Body.String())
		}
	}
	if object.Silences[0]["scope_name"] != "current" || object.Silences[0]["active"] != true || object.Silences[1]["active"] != false {
		t.Fatalf("list=%s", response.Body.String())
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_silence_audit BEFORE INSERT ON audit_events WHEN NEW.action IN ('alert_silence.create','alert_silence.delete') BEGIN SELECT RAISE(ABORT,'audit failed'); END`); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"reason": "rollback", "scope_kind": "all", "ends_at": now.Add(time.Hour)})
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/alert-silences", string(body), admin, csrf, publicURL), 500)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/alert-silences/"+active, "", admin, csrf, publicURL), 500)
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM alert_silences WHERE reason='rollback'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("create rollback=%d %v", count, err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM alert_silences WHERE id=?", active).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delete rollback=%d %v", count, err)
	}
}

func TestSilenceEventHTTPBoolean(t *testing.T) {
	f := newHTTP(t, 1)
	admin, _ := login(t, f, "admin-two")
	const id = "00000000-0000-4000-8000-000000000077"
	if _, err := f.db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,started_at,rule_name,node_name,rule_snapshot,silenced) VALUES(?,'deleted','deleted','firing',?,'rule','node','{}',1)`, id, timekey.Format(time.Now())); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, "GET", "/api/v1/alerts/"+id, "", admin, "", "")
	assertHTTPStatus(t, response, 200)
	if decodeObject(t, response)["silenced"] != true {
		t.Fatalf("event=%s", response.Body.String())
	}
	response = request(t, f.handler, "GET", "/api/v1/alerts?state=firing", "", admin, "", "")
	assertHTTPStatus(t, response, 200)
	var page struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Events) != 1 || page.Events[0]["silenced"] != true {
		t.Fatalf("page=%s %v", response.Body.String(), err)
	}
}
