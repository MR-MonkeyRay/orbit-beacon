package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

type publicListFixture struct {
	Nodes       []map[string]json.RawMessage `json:"nodes"`
	HasMore     bool                         `json:"has_more"`
	NextOffset  *int                         `json:"next_offset"`
	GeneratedAt string                       `json:"generated_at"`
}

func agentHTTPCall(t *testing.T, f *agentFixture, method, route, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, publicURL+route, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	return response
}

func loginAgent(t *testing.T, f *agentFixture, username string) (*http.Cookie, string) {
	t.Helper()
	var users int
	if err := f.db.QueryRow("SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users == 0 {
		if _, err := f.auth.BootstrapAdministrator(context.Background(), "admin", password); err != nil {
			t.Fatal(err)
		}
		if _, err := f.auth.CreateUser(context.Background(), "viewer", password, "viewer"); err != nil {
			t.Fatal(err)
		}
	}
	response := agentHTTPCall(t, f, http.MethodPost, "/api/v1/session", fmt.Sprintf(`{"username":%q,"password":%q}`, username, password), nil, "", publicURL)
	assertHTTPStatus(t, response, http.StatusOK)
	var result struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.HttpOnly && cookie.Value != "" {
			return cookie, result.CSRFToken
		}
	}
	t.Fatal("login did not set an HttpOnly cookie")
	return nil, ""
}

func publicList(t *testing.T, f *agentFixture, path string, cookie *http.Cookie) (publicListFixture, []byte) {
	t.Helper()
	response := agentHTTPCall(t, f, http.MethodGet, path, "", cookie, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var result publicListFixture
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(envelope))
	for key := range envelope {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"generated_at", "has_more", "next_offset", "nodes"}) {
		t.Fatalf("public envelope keys: %v", keys)
	}
	if result.Nodes == nil {
		t.Fatal("public collection is null")
	}
	if generated, err := time.Parse(time.RFC3339Nano, result.GeneratedAt); err != nil || generated.After(f.now) || f.now.Sub(generated) >= 2*time.Second {
		t.Fatalf("generated_at=%q err=%v", result.GeneratedAt, err)
	}
	if response.Header().Get("Set-Cookie") != "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("anonymous response mutates cookies or permits caching")
	}
	return result, append([]byte(nil), response.Body.Bytes()...)
}

func publicNodeKeys(t *testing.T, node map[string]json.RawMessage) {
	t.Helper()
	got := make([]string, 0, len(node))
	for key := range node {
		got = append(got, key)
	}
	sort.Strings(got)
	want := []string{"cpu_percent", "load1", "memory_percent", "name", "provider", "region", "status", "uptime_seconds"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public node keys=%v want=%v", got, want)
	}
}

func publicSettingsPatch(t *testing.T, f *agentFixture, id, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	return agentHTTPCall(t, f, http.MethodPatch, "/api/v1/nodes/"+id+"/public-settings", body, cookie, csrf, origin)
}

func publicSettingsObject(t *testing.T, response *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	assertHTTPStatus(t, response, http.StatusOK)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"enabled", "name", "provider", "region"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("public settings keys=%v want=%v", keys, want)
	}
	return object
}

func TestPublicSettingsHTTPAuthorizationValidationAndIsolation(t *testing.T) {
	f := newAgentHTTP(t, 5)
	id, _, _ := enrollHTTP(t, f, "private-203.0.113.7", 1)
	admin, csrf := loginAgent(t, f, "admin")
	viewer, _ := loginAgent(t, f, "viewer")
	path := "/api/v1/nodes/" + id + "/public-settings"

	apiError(t, agentHTTPCall(t, f, http.MethodGet, path, "", nil, "", ""), http.StatusUnauthorized, "unauthenticated")
	publicSettingsObject(t, agentHTTPCall(t, f, http.MethodGet, path, "", viewer, "", ""))
	apiError(t, publicSettingsPatch(t, f, id, `{"enabled":true,"name":"viewer"}`, viewer, "", publicURL), http.StatusForbidden, "forbidden")
	apiError(t, publicSettingsPatch(t, f, id, `{"enabled":true,"name":"wrong-origin"}`, admin, csrf, "https://attacker.example.test"), http.StatusForbidden, "forbidden")
	apiError(t, publicSettingsPatch(t, f, id, `{"enabled":true,"name":"missing-csrf"}`, admin, "", publicURL), http.StatusForbidden, "forbidden")

	for _, test := range []struct {
		name  string
		body  string
		field string
	}{
		{"empty published name", `{"enabled":true,"name":""}`, "name"},
		{"name too long", fmt.Sprintf(`{"enabled":true,"name":%q}`, strings.Repeat("中", 86)), "name"},
		{"control character", `{"enabled":true,"name":"bad\u0000name"}`, "name"},
		{"invalid UTF-8 name", string([]byte{'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', '}'}), "name"},
		{"region too long", fmt.Sprintf(`{"region":%q}`, strings.Repeat("中", 43)), "region"},
		{"provider control", `{"provider":"bad\nprovider"}`, "provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := apiError(t, publicSettingsPatch(t, f, id, test.body, admin, csrf, publicURL), http.StatusBadRequest, "validation_failed")
			var got map[string]string
			if err := json.Unmarshal(fields["fields"], &got); err != nil || got[test.field] == "" {
				t.Fatalf("validation fields=%s err=%v", fields["fields"], err)
			}
		})
	}
	apiError(t, publicSettingsPatch(t, f, id, `{"enabled":true,"name":"ok","unexpected":true}`, admin, csrf, publicURL), http.StatusBadRequest, "invalid_json")

	settings := publicSettingsPatch(t, f, id, `{"enabled":true,"name":"  Tokyo public  ","region":"  Tokyo ","provider":" Example Cloud "}`, admin, csrf, publicURL)
	object := publicSettingsObject(t, settings)
	for key, want := range map[string]string{"name": "Tokyo public", "region": "Tokyo", "provider": "Example Cloud"} {
		var got string
		if err := json.Unmarshal(object[key], &got); err != nil || got != want {
			t.Errorf("%s=%q err=%v want=%q", key, got, err, want)
		}
	}
	if enabled, ok := object["enabled"]; !ok || string(enabled) != "true" {
		t.Fatalf("enabled=%s", object["enabled"])
	}
	// Omitted and explicit null preserve values; an empty string clears a field.
	object = publicSettingsObject(t, publicSettingsPatch(t, f, id, `{"name":null,"region":"","provider":null}`, admin, csrf, publicURL))
	var name, region, provider string
	_ = json.Unmarshal(object["name"], &name)
	_ = json.Unmarshal(object["region"], &region)
	_ = json.Unmarshal(object["provider"], &provider)
	if name != "Tokyo public" || region != "" || provider != "Example Cloud" {
		t.Fatalf("null/empty patch changed wrong values: name=%q region=%q provider=%q", name, region, provider)
	}

	pending, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "pending-private"})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, publicSettingsPatch(t, f, pending.ID, `{"enabled":true,"name":"Pending public"}`, admin, csrf, publicURL), http.StatusOK)
	var sessionsBefore, sessionsAfter int
	if err := f.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessionsBefore); err != nil {
		t.Fatal(err)
	}
	anonymous, _ := publicList(t, f, "/api/public/v1/nodes", nil)
	if err := f.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessionsAfter); err != nil {
		t.Fatal(err)
	}
	if sessionsBefore != sessionsAfter || len(anonymous.Nodes) != 1 {
		t.Fatalf("anonymous public settings/list mutated sessions or exposed pending node: before=%d after=%d nodes=%d", sessionsBefore, sessionsAfter, len(anonymous.Nodes))
	}
}

func TestPublicHTTPProjectionPrivacyAndTemporalBoundaries(t *testing.T) {
	f := newAgentHTTP(t, 4)
	id, token, _ := enrollHTTP(t, f, "private-2001:db8::7", 2)
	admin, csrf := loginAgent(t, f, "admin")
	viewer, _ := loginAgent(t, f, "viewer")
	if _, err := f.db.Exec(`UPDATE nodes SET notes='private-note-203.0.113.8', region='private-region', provider='private-provider' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO node_tags(node_id,tag) VALUES (?,'private-tag-2001:db8::8')`, id); err != nil {
		t.Fatal(err)
	}
	if response := publicSettingsPatch(t, f, id, `{"enabled":true,"name":"Tokyo node","region":"Tokyo","provider":"Example cloud"}`, admin, csrf, publicURL); response.Code != http.StatusOK {
		t.Fatalf("publish returned %d: %s", response.Code, response.Body)
	}

	unknown, initialBytes := publicList(t, f, "/api/public/v1/nodes", nil)
	if len(unknown.Nodes) != 1 || unknown.Nodes[0]["status"] == nil || string(unknown.Nodes[0]["status"]) != `"unknown"` {
		t.Fatalf("unenlived publication=%s", initialBytes)
	}
	publicNodeKeys(t, unknown.Nodes[0])
	for _, key := range []string{"cpu_percent", "memory_percent", "load1", "uptime_seconds"} {
		if string(unknown.Nodes[0][key]) != "null" {
			t.Errorf("unknown %s=%s, want null", key, unknown.Nodes[0][key])
		}
	}

	base := f.now
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "private-session", 25)), 1)
	f.now = f.now.Add(2 * time.Second) // Expire the pre-report rendered page.
	anonymous, anonymousBytes := publicList(t, f, "/api/public/v1/nodes", nil)
	adminResult, adminBytes := publicList(t, f, "/api/public/v1/nodes", admin)
	viewerResult, viewerBytes := publicList(t, f, "/api/public/v1/nodes", viewer)
	forgedResult, forgedBytes := publicList(t, f, "/api/public/v1/nodes", &http.Cookie{Name: "__Host-orbitbeacon_session", Value: "forged"})
	if len(anonymous.Nodes) != 1 || len(adminResult.Nodes) != 1 || len(viewerResult.Nodes) != 1 || len(forgedResult.Nodes) != 1 {
		t.Fatalf("public visibility differs by session: anonymous=%d admin=%d viewer=%d forged=%d", len(anonymous.Nodes), len(adminResult.Nodes), len(viewerResult.Nodes), len(forgedResult.Nodes))
	}
	for _, got := range [][]byte{adminBytes, viewerBytes, forgedBytes} {
		if !reflect.DeepEqual(got, anonymousBytes) {
			t.Fatalf("authenticated public response differs from anonymous: %s vs %s", got, anonymousBytes)
		}
	}
	node := anonymous.Nodes[0]
	publicNodeKeys(t, node)
	for key, want := range map[string]string{"name": `"Tokyo node"`, "region": `"Tokyo"`, "provider": `"Example cloud"`, "status": `"online"`, "cpu_percent": "25", "memory_percent": "50", "load1": "0.5", "uptime_seconds": "100"} {
		if string(node[key]) != want {
			t.Errorf("public %s=%s want %s", key, node[key], want)
		}
	}
	for _, sentinel := range []string{"private-2001:db8::7", "private-note-203.0.113.8", "private-region", "private-provider", "private-tag-2001:db8::8", "identical-host", "private-session", "cpu-1", "memory-1", "sample_id", "token_hash", "installation-private"} {
		if strings.Contains(string(anonymousBytes), sentinel) {
			t.Errorf("anonymous response leaked private sentinel %q: %s", sentinel, anonymousBytes)
		}
	}

	f.now = base.Add(45 * time.Second)
	fresh, _ := publicList(t, f, "/api/public/v1/nodes", nil)
	if fresh.Nodes[0]["status"] == nil || string(fresh.Nodes[0]["status"]) != `"online"` || string(fresh.Nodes[0]["cpu_percent"]) != "25" {
		t.Fatalf("metric exact freshness boundary: %s", fresh.Nodes[0])
	}
	f.now = base.Add(45*time.Second + time.Millisecond)
	stale, _ := publicList(t, f, "/api/public/v1/nodes?limit=49", nil)
	if string(stale.Nodes[0]["status"]) != `"online"` || string(stale.Nodes[0]["cpu_percent"]) != "null" {
		t.Fatalf("source expiry should not make node offline: %s", stale.Nodes[0])
	}
	f.now = base.Add(180 * time.Second)
	boundary, _ := publicList(t, f, "/api/public/v1/nodes", nil)
	if string(boundary.Nodes[0]["status"]) != `"online"` {
		t.Fatalf("180-second liveness boundary: %s", boundary.Nodes[0])
	}
	f.now = base.Add(180*time.Second + time.Nanosecond)
	offline, _ := publicList(t, f, "/api/public/v1/nodes?limit=48", nil)
	if string(offline.Nodes[0]["status"]) != `"offline"` || string(offline.Nodes[0]["cpu_percent"]) != "null" {
		t.Fatalf("liveness expiry: %s", offline.Nodes[0])
	}

	// A failed report cannot refresh the accepted report's last_seen or snapshot.
	f.now = base.Add(181 * time.Second)
	failed := strings.Replace(reportBody(1, 2, "private-session", 99), `"cpu_percent":99`, `"cpu_percent":101`, 1)
	if response := agentRequest(t, f, "/api/agent/v1/report", token, failed); response.Code == http.StatusOK {
		t.Fatal("invalid report was accepted")
	}
	f.now = base.Add(180*time.Second + time.Nanosecond)
	afterFailure, _ := publicList(t, f, "/api/public/v1/nodes?limit=48", nil)
	if string(afterFailure.Nodes[0]["status"]) != `"offline"` {
		t.Fatalf("failed report refreshed public liveness: %s", afterFailure.Nodes[0])
	}
}

func TestPublicHTTPRotationRestartPaginationAndWithdrawal(t *testing.T) {
	f := newAgentHTTP(t, 8)
	admin, csrf := loginAgent(t, f, "admin")
	var ids []string
	var tokens []string
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(time.Second)
		id, token, _ := enrollHTTP(t, f, fmt.Sprintf("private-node-%d", i), byte(i+10))
		ids = append(ids, id)
		tokens = append(tokens, token)
		body := fmt.Sprintf(`{"enabled":true,"name":"Public %d","region":"Region %d","provider":"Provider"}`, i, i)
		assertHTTPStatus(t, publicSettingsPatch(t, f, id, body, admin, csrf, publicURL), http.StatusOK)
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokens[0], reportBody(1, 1, "first-session", 10)), 1)

	first, _ := publicList(t, f, "/api/public/v1/nodes?limit=1&offset=0", nil)
	if len(first.Nodes) != 1 || !first.HasMore || first.NextOffset == nil || *first.NextOffset != 1 {
		t.Fatalf("first public page=%+v", first)
	}
	next, _ := publicList(t, f, "/api/public/v1/nodes?limit=1&offset=1", nil)
	if len(next.Nodes) != 1 || next.Nodes[0]["name"] == nil || string(next.Nodes[0]["name"]) != `"Public 1"` {
		t.Fatalf("stable public pagination=%+v", next)
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?offset=-1", "?limit=nope"} {
		apiError(t, agentHTTPCall(t, f, http.MethodGet, "/api/public/v1/nodes"+query, "", nil, "", ""), http.StatusBadRequest, "validation_failed")
	}
	method := agentHTTPCall(t, f, http.MethodPost, "/api/public/v1/nodes", "", nil, "", "")
	assertHTTPStatus(t, method, http.StatusMethodNotAllowed)
	if method.Header().Get("Allow") != http.MethodGet {
		t.Errorf("public method Allow=%q", method.Header().Get("Allow"))
	}
	assertHTTPStatus(t, agentHTTPCall(t, f, http.MethodGet, "/api/public/v1/secret", "", nil, "", ""), http.StatusNotFound)

	// Rotation invalidates the old in-memory projection until a new identity reports.
	oldToken := tokens[0]
	ticket, err := f.enrollments.CreateSingle(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	newToken := agentSecret(42)
	claim, err := json.Marshal(v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "installation-private-node-0", NodeToken: newToken, PreviousNodeToken: oldToken, System: v1.System{Hostname: "rotation-private-host", OS: "linux", Arch: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, string(claim)), http.StatusOK)
	f.now = f.now.Add(2 * time.Second) // Expire the page computed before rotation.
	rotated, _ := publicList(t, f, "/api/public/v1/nodes?limit=1&offset=0", nil)
	if string(rotated.Nodes[0]["status"]) != `"unknown"` || string(rotated.Nodes[0]["cpu_percent"]) != "null" {
		t.Fatalf("rotation retained old public identity: %s", rotated.Nodes[0])
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", newToken, reportBody(1, 1, "new-session", 30)), 1)

	// Reconstructing ingest loses only the in-memory resource snapshot; durable
	// last_seen still supplies liveness.
	f.ingest = ingest.NewService(f.db, ingest.Options{Now: func() time.Time { return f.now }})
	f.handler, err = server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := publicList(t, f, "/api/public/v1/nodes?limit=1&offset=0", nil)
	if string(restarted.Nodes[0]["status"]) != `"online"` || string(restarted.Nodes[0]["cpu_percent"]) != "null" {
		t.Fatalf("restart public projection=%s", restarted.Nodes[0])
	}

	// Withdrawal is immediate and removes the candidate from pagination.
	assertHTTPStatus(t, publicSettingsPatch(t, f, ids[0], `{"enabled":false}`, admin, csrf, publicURL), http.StatusOK)
	withdrawn, _ := publicList(t, f, "/api/public/v1/nodes?limit=100&offset=0", nil)
	if len(withdrawn.Nodes) != 2 {
		t.Fatalf("withdrawal left node visible: %+v", withdrawn)
	}
	for _, node := range withdrawn.Nodes {
		if string(node["name"]) == `"Public 0"` {
			t.Fatal("withdrawn public node still listed")
		}
	}
}
