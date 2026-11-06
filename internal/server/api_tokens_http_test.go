package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
	"os"
)

func apiTokenRequest(t *testing.T, h http.Handler, verb, path, authorization string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(verb, publicURL+path, strings.NewReader(`{"name":"modified","password":"replacement-password"}`))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}
func createHTTPAPIToken(t *testing.T, f fixture) (auth.APIToken, string, *http.Cookie, string) {
	t.Helper()
	cookie, csrf := login(t, f, "admin")
	response := request(t, f.handler, "POST", "/api/v1/api-tokens", `{"name":"scripts","expires_in_days":30,"password":"`+password+`"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 201)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("creation cached")
	}
	var created struct {
		auth.APIToken
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.APIToken, created.Token, cookie, csrf
}
func assertLiteralKeys(t *testing.T, value any, expected ...string) {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("not object: %T", value)
	}
	actual := make([]string, 0, len(object))
	for k := range object {
		actual = append(actual, k)
	}
	slices.Sort(actual)
	slices.Sort(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("keys=%v want=%v", actual, expected)
	}
}
func tokenObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	assertHTTPStatus(t, response, 200)
	var object map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestAPITokenHTTPAuthenticationAndCookiePrecedence(t *testing.T) {
	f := newHTTP(t, 5)
	metadata, token, cookie, _ := createHTTPAPIToken(t, f)
	for _, path := range []string{"/api/v1/nodes", "/api/v1/alerts", "/api/v1/alerts/summary", "/api/v1/groups"} {
		assertHTTPStatus(t, apiTokenRequest(t, f.handler, "GET", path, "Bearer "+token, cookie), 200)
	}
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/users", "Bearer "+token, cookie), 403, "token_forbidden")
	// 令牌格式错误时不回退 Cookie。
	for _, value := range []string{"Bearer " + token + "=", "Bearer obt_invalid", "Bearer obt_" + strings.Repeat("A", 43), "Bearer " + token + ", Bearer " + token} {
		apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", value, cookie), 401, "unauthenticated")
	}
	// 其他 Authorization 不是令牌：单独使用无权限，也不影响 Cookie 会话和管理路由。
	for _, value := range []string{token, "Basic " + token, "bearer " + token, "Bearer  " + token, "Bearer invalid", "Basic cHJveHk6c2VjcmV0"} {
		apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", value, nil), 401, "unauthenticated")
		assertHTTPStatus(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/users", value, cookie), 200)
	}
	empty := httptest.NewRequest("GET", publicURL+"/api/v1/nodes", nil)
	empty.Header["Authorization"] = []string{""}
	got := httptest.NewRecorder()
	f.handler.ServeHTTP(got, empty)
	apiError(t, got, 401, "unauthenticated")
	mixed := httptest.NewRequest("GET", publicURL+"/api/v1/nodes", nil)
	mixed.Header.Add("Authorization", "Basic cHJveHk6c2VjcmV0")
	mixed.Header.Add("Authorization", "Bearer "+token)
	mixed.AddCookie(cookie)
	got = httptest.NewRecorder()
	f.handler.ServeHTTP(got, mixed)
	apiError(t, got, 401, "unauthenticated")
	multiple := httptest.NewRequest("GET", publicURL+"/api/v1/nodes", nil)
	multiple.Header.Add("Authorization", "Bearer "+token)
	multiple.Header.Add("Authorization", "Bearer "+token)
	multiple.AddCookie(cookie)
	got = httptest.NewRecorder()
	f.handler.ServeHTTP(got, multiple)
	apiError(t, got, 401, "unauthenticated")
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes?token="+token, "", nil), 401, "unauthenticated")
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", "", &http.Cookie{Name: "token", Value: token}), 401, "unauthenticated")
	if _, err := f.db.Exec(`UPDATE users SET must_change_password=1 WHERE id=?`, metadata.CreatedBy); err != nil {
		t.Fatal(err)
	}
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", "Bearer "+token, cookie), 403, "password_change_required")
	if _, err := f.db.Exec(`UPDATE users SET must_change_password=0 WHERE id=?`, metadata.CreatedBy); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE api_tokens SET expires_at=? WHERE id=?`, timekey.Format(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)), metadata.ID); err != nil {
		t.Fatal(err)
	}
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", "Bearer "+token, cookie), 401, "unauthenticated")
	if _, err := f.db.Exec(`UPDATE api_tokens SET expires_at=NULL WHERE id=?`, metadata.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.RevokeAPIToken(context.Background(), metadata.ID); err != nil {
		t.Fatal(err)
	}
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", "Bearer "+token, cookie), 401, "unauthenticated")
}

func TestAPITokenHTTPFieldsAndWrites(t *testing.T) {
	f := newAgentHTTP(t, 5)
	ctx := context.Background()
	if _, err := f.auth.CreateUser(ctx, "admin", password, false); err != nil {
		t.Fatal(err)
	}
	fixture := fixture{handler: f.handler, auth: f.auth, nodes: f.nodes, db: f.db}
	metadata, token, cookie, _ := createHTTPAPIToken(t, fixture)
	id, agentToken, _ := enrollHTTP(t, f, "token-node", 81)
	body := strings.Replace(reportBody(1, 1, "private-token-session", 25), `"metrics":`, `"system":{"hostname":"private-hostname","os":"linux","arch":"amd64","agent_version":"v1.2.3"},"metrics":`, 1)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", agentToken, body), 1)
	runtime := v1.DefaultRuntimeSettings()
	runtime.ProbeTargets = []string{"https://private-target.example"}
	encoded, _ := json.Marshal(runtime)
	if _, err := f.db.Exec(`UPDATE nodes SET notes='private-notes',agent_runtime=? WHERE id=?`, string(encoded), id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO probe_latest(node_id,target,ok,error,checked_at) VALUES(?,?,0,'refused',?)`, id, runtime.ProbeTargets[0], timekey.Format(f.now)); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/nodes/" + id
	bearer := "Bearer " + token
	expectedNode := []string{"stopped", "id", "name", "status", "expires_on", "region", "provider", "group_id", "tags", "created_at", "last_seen", "presence", "agent_version"}
	detail := apiTokenRequest(t, f.handler, "GET", base, bearer, cookie)
	assertLiteralKeys(t, tokenObject(t, detail), expectedNode...)
	list := tokenObject(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes?limit=1", bearer, cookie))
	assertLiteralKeys(t, list, "nodes", "has_more")
	assertLiteralKeys(t, list["nodes"].([]any)[0], expectedNode...)
	latest := apiTokenRequest(t, f.handler, "GET", base+"/latest", bearer, cookie)
	assertLiteralKeys(t, tokenObject(t, latest), "received_at", "sampled_at", "seq", "metrics", "server_time")
	assertLiteralKeys(t, tokenObject(t, latest)["metrics"], "disk_io", "pressure", "temperature_celsius", "cpu_percent", "load1", "load5", "load15", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes", "uptime_seconds", "disks", "networks", "metric_status", "sources")
	probes := apiTokenRequest(t, f.handler, "GET", base+"/probes", bearer, cookie)
	probeObject := tokenObject(t, probes)
	assertLiteralKeys(t, probeObject, "probes")
	assertLiteralKeys(t, probeObject["probes"].([]any)[0], "status", "latency_ms", "status_code", "checked_at", "uptime_24h", "checks_24h")
	for _, response := range []*httptest.ResponseRecorder{detail, latest, probes} {
		for _, private := range []string{`"notes"`, `"system"`, `"runtime"`, `"target"`, `"error"`, `"hostname"`, "private-notes", "private-hostname", "private-target", "private-error", token} {
			if strings.Contains(response.Body.String(), private) {
				t.Fatalf("leaked %s", private)
			}
		}
	}
	for _, suffix := range []string{"/history?metric=temperature_celsius&range=1h", "/history?metric=disk_read_bytes_per_second&range=1h", "/history?metric=pressure_io_percent&range=1h", "/history?metric=cpu_percent&range=1h", "/history?metric=disk_used_percent&range=1h&disk=/data:one", "/history?metric=inode_used_percent&range=1h&disk=/data:one", "/availability", "/traffic"} {
		result := tokenObject(t, apiTokenRequest(t, f.handler, "GET", base+suffix, bearer, cookie))
		admin := tokenObject(t, request(t, f.handler, "GET", base+suffix, "", cookie, "", ""))
		switch {
		case strings.HasPrefix(suffix, "/history"):
			assertLiteralKeys(t, result, "metric", "unit", "range", "step_seconds", "points")
		case suffix == "/availability":
			assertLiteralKeys(t, result, "days", "percent")
			assertLiteralKeys(t, result["days"].([]any)[0], "day", "online_seconds", "offline_seconds", "unknown_seconds", "percent")
		case suffix == "/traffic":
			assertLiteralKeys(t, result, "billing_mode", "billing_day", "quota_bytes", "cycle", "daily", "updated_at")
			assertLiteralKeys(t, result["cycle"], "used_bytes", "start", "end", "rx_bytes", "tx_bytes", "total_bytes")
		}
		if !reflect.DeepEqual(result, admin) {
			t.Fatalf("different %s response", suffix)
		}
	}
	before := tokenDomainSnapshot(t, fixture)
	for _, path := range []string{"/api/v1/nodes", base, base + "/latest", base + "/history", base + "/availability", base + "/traffic", base + "/probes", "/api/v1/alerts", "/api/v1/alerts/summary", "/api/v1/alerts/" + id, "/api/v1/groups"} {
		for _, verb := range []string{"POST", "PATCH", "DELETE"} {
			apiError(t, apiTokenRequest(t, f.handler, verb, path, bearer, cookie), 403, "token_forbidden")
		}
	}
	if after := tokenDomainSnapshot(t, fixture); !reflect.DeepEqual(before, after) {
		t.Fatal("token writes changed domain data")
	}
	var notes, name string
	if err := f.db.QueryRow(`SELECT name,notes FROM nodes WHERE id=?`, id).Scan(&name, &notes); err != nil || name != "token-node" || notes != "private-notes" {
		t.Fatal("writes changed node", err)
	}
	tokens := tokenObject(t, request(t, f.handler, "GET", "/api/v1/api-tokens", "", cookie, "", ""))
	assertLiteralKeys(t, tokens, "api_tokens")
	assertLiteralKeys(t, tokens["api_tokens"].([]any)[0], "id", "name", "created_by", "created_by_username", "created_at", "expires_at", "last_used_at", "rate_limit_per_minute")
	if strings.Contains(stringMustJSON(t, tokens), token) {
		t.Fatal("list leaked plaintext")
	}
	if metadata.ID == "" {
		t.Fatal("missing id")
	}
}

// 告警和分组的等价性见 TestAPITokenHTTPAlertsGroupsAndLiteralFields。
func TestAPITokenSeriesRoutesMatchAdministrator(t *testing.T) {
	f := newAgentHTTP(t, 5)
	if _, err := f.auth.CreateUser(context.Background(), "admin", password, false); err != nil {
		t.Fatal(err)
	}
	_, token, cookie, _ := createHTTPAPIToken(t, fixture{handler: f.handler, auth: f.auth, nodes: f.nodes, db: f.db})
	id, agentToken, _ := enrollHTTP(t, f, "match-node", 82)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", agentToken, reportBody(1, 1, "match-session", 25)), 1)
	base := "/api/v1/nodes/" + id
	for _, path := range []string{base + "/history?metric=cpu_percent&range=1h", base + "/availability", base + "/traffic"} {
		bearer := apiTokenRequest(t, f.handler, http.MethodGet, path, "Bearer "+token, nil)
		session := request(t, f.handler, http.MethodGet, path, "", cookie, "", "")
		assertHTTPStatus(t, bearer, http.StatusOK)
		assertHTTPStatus(t, session, http.StatusOK)
		if bearer.Body.String() != session.Body.String() {
			t.Errorf("%s\n token: %s\n admin: %s", path, bearer.Body, session.Body)
		}
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAPITokenHTTPManagementValidationAndNoSecretLogs(t *testing.T) {
	f := newBackupHTTP(t)
	metadata, token, cookie, csrf := createHTTPAPIToken(t, f.fixture)
	for _, days := range []string{"0", "31", "\"30\"", "30.0", "true", "[]", "{}"} {
		apiError(t, request(t, f.handler, "POST", "/api/v1/api-tokens", `{"name":"bad","expires_in_days":`+days+`,"password":"`+password+`"}`, cookie, csrf, publicURL), 400, "validation_failed")
	}
	apiError(t, request(t, f.handler, "POST", "/api/v1/api-tokens", `{"name":"bad-password","expires_in_days":null,"password":"wrong"}`, cookie, csrf, publicURL), 400, "invalid_old_password")
	id := backupHTTPMissingID
	seedBackupHTTPArtifact(t, f, id)
	response := apiTokenRequest(t, f.handler, "GET", "/api/v1/tasks/"+id+"/download", "Bearer "+token, cookie)
	apiError(t, response, 403, "token_forbidden")
	if strings.Contains(response.Body.String(), "0123456789") || response.Header().Get("Content-Disposition") != "" {
		t.Fatal("archive leaked")
	}
	apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes?token="+token, "Bearer "+token, cookie)
	apiTokenRequest(t, f.handler, "GET", "/api/v1/users", "Bearer "+token, cookie)
	if strings.Contains(f.logs.String(), token) {
		t.Fatal("plaintext logged")
	}
	rows, err := f.db.Query(`SELECT details FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var details string
		if err = rows.Scan(&details); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(details, token) {
			t.Fatal("plaintext audited")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	response = request(t, f.handler, "DELETE", "/api/v1/api-tokens/"+metadata.ID, "", cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 204)
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", "Bearer "+token, cookie), 401, "unauthenticated")
}

func TestAPITokenHTTPCreateLimitAndAllExpiryChoices(t *testing.T) {
	f := newHTTP(t, 1)
	cookie, csrf := login(t, f, "admin")
	var owner string
	if err := f.db.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	for i, days := range []string{"30", "90", "365", "null"} {
		response := request(t, f.handler, "POST", "/api/v1/api-tokens", `{"name":"choice","expires_in_days":`+days+`,"password":"`+password+`"}`, cookie, csrf, publicURL)
		assertHTTPStatus(t, response, 201)
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cached")
		}
		var created struct {
			auth.APIToken
			Token string `json:"token"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if days == "null" {
			if created.ExpiresAt != nil {
				t.Fatal("never expires")
			}
		} else {
			expiry, err := timekey.Parse(*created.ExpiresAt)
			if err != nil {
				t.Fatal(err)
			}
			expected := []int{30, 90, 365}[i]
			if !expiry.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC).Add(time.Duration(expected) * 24 * time.Hour)) {
				t.Fatal("expiry mismatch")
			}
		}
	}
	// 已有四枚，再插入十六枚过期令牌；过期项仍占全局名额。
	for i := 0; i < 16; i++ {
		if _, err := f.db.Exec(`INSERT INTO api_tokens(id,name,token_hash,created_by,created_at,expires_at) VALUES(?,?,randomblob(32),?,?,?)`, fmt.Sprintf("limit-%d", i), "expired", owner, timekey.Format(time.Now()), timekey.Format(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))); err != nil {
			t.Fatal(err)
		}
	}
	apiError(t, request(t, f.handler, "POST", "/api/v1/api-tokens", `{"name":"overflow","expires_in_days":null,"password":"`+password+`"}`, cookie, csrf, publicURL), 409, "api_token_limit")
}

func TestAPITokenHTTPAlertsGroupsAndLiteralFields(t *testing.T) {
	f := newHTTP(t, 1)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	id := "00000000-0000-4000-8000-000000000001"
	if _, err := f.db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,rule_name,node_name,rule_snapshot) VALUES(?,'deleted-rule','deleted-node','firing',90,?,'retained rule','retained node','{"metric":"cpu_percent","severity":"critical"}')`, id, timekey.Format(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	group := createHTTPGroup(t, f, cookie, csrf, "token-group")
	eventKeys := []string{"id", "rule_id", "node_id", "rule_name", "metric", "severity", "node_name", "state", "value", "started_at", "resolved_at", "last_notified_at", "silenced"}
	bearer := "Bearer " + token
	for _, path := range []string{"/api/v1/alerts", "/api/v1/alerts/summary", "/api/v1/alerts/" + id, "/api/v1/groups"} {
		got := tokenObject(t, apiTokenRequest(t, f.handler, "GET", path, bearer, cookie))
		admin := tokenObject(t, request(t, f.handler, "GET", path, "", cookie, "", ""))
		if !reflect.DeepEqual(got, admin) {
			t.Fatalf("changed shape: %s", path)
		}
		switch path {
		case "/api/v1/alerts":
			assertLiteralKeys(t, got, "events", "has_more")
			assertLiteralKeys(t, got["events"].([]any)[0], eventKeys...)
		case "/api/v1/alerts/summary":
			assertLiteralKeys(t, got, "firing", "newest")
			assertLiteralKeys(t, got["firing"], "total", "info", "warning", "critical", "unknown")
			assertLiteralKeys(t, got["newest"].([]any)[0], eventKeys...)
		case "/api/v1/groups":
			assertLiteralKeys(t, got, "groups")
			assertLiteralKeys(t, got["groups"].([]any)[0], "id", "name")
			if got["groups"].([]any)[0].(map[string]any)["id"] != group {
				t.Fatal("wrong group")
			}
		default:
			assertLiteralKeys(t, got, eventKeys...)
		}
	}
}

func TestAPITokenHTTPNodePaginationKeepsEnvelope(t *testing.T) {
	f := newHTTP(t, 3)
	_, token, cookie, _ := createHTTPAPIToken(t, f)
	for _, name := range []string{"first", "second"} {
		if _, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	first := tokenObject(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes?limit=1", "Bearer "+token, cookie))
	assertLiteralKeys(t, first, "nodes", "has_more", "next_offset")
	if first["has_more"] != true || first["next_offset"] != float64(1) {
		t.Fatal("pagination changed")
	}
	second := tokenObject(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes?limit=1&offset=1", "Bearer "+token, cookie))
	assertLiteralKeys(t, second, "nodes", "has_more")
	if second["has_more"] != false || len(second["nodes"].([]any)) != 1 {
		t.Fatal("last page changed")
	}
}

func tokenDomainSnapshot(t *testing.T, f fixture) []string {
	t.Helper()
	var result []string
	for _, query := range []string{
		"SELECT id,username,password_hash,must_change_password FROM users ORDER BY id",
		"SELECT id,name FROM groups ORDER BY id",
		"SELECT id,name,status,notes,group_id,agent_runtime FROM nodes ORDER BY id",
		"SELECT id,name,enabled,metric FROM alert_rules ORDER BY id",
		"SELECT id,rule_id,node_id,state FROM alert_events ORDER BY id",
	} {
		rows, err := f.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result = append(result, stringMustJSON(t, values))
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err = rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestAPITokenDiskHistoryQueriesMatchAdministrator(t *testing.T) {
	f := newAgentHTTP(t, 5)
	ctx := context.Background()
	if _, err := f.auth.CreateUser(ctx, "admin", password, false); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	hs, err := history.Open(ctx, dir, f.db, history.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, History: hs, PublicURL: publicURL, Now: func() time.Time { return f.now }}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown(ctx)
	f.handler = h
	_, token, cookie, _ := createHTTPAPIToken(t, fixture{handler: h, auth: f.auth, nodes: f.nodes, db: f.db})
	node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "disk-history"})
	if err != nil {
		t.Fatal(err)
	}
	hs.Samples() <- history.Sample{NodeID: node.ID, ReceivedAt: f.now, Values: map[string]float64{"disk_used_percent:/data:one": 25, "inode_used_percent:/data:one": 75}}
	if err := hs.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/nodes/" + node.ID + "/history"
	for metric, value := range map[string]float64{"disk_used_percent": 25, "inode_used_percent": 75} {
		path := base + "?metric=" + metric + "&range=1h&disk=/data:one"
		bearer := apiTokenRequest(t, h, "GET", path, "Bearer "+token, nil)
		admin := request(t, h, "GET", path, "", cookie, "", "")
		object := tokenObject(t, bearer)
		assertLiteralKeys(t, object, "metric", "unit", "range", "step_seconds", "points")
		if bearer.Body.String() != admin.Body.String() {
			t.Fatalf("token/admin differ: %s %s", bearer.Body, admin.Body)
		}
		points := object["points"].([]any)
		if object["metric"] != metric || len(points) != 1 || points[0].(map[string]any)["avg"] != value {
			t.Fatalf("body=%s", bearer.Body)
		}
	}
	missing := tokenObject(t, apiTokenRequest(t, h, "GET", base+"?metric=inode_used_percent&range=1h&disk=/missing", "Bearer "+token, nil))
	if len(missing["points"].([]any)) != 0 {
		t.Fatalf("missing=%v", missing)
	}
	for _, query := range []string{"?metric=load1&range=1h&disk=/", "?metric=disk_used_percent&range=1h&disk=relative", "?metric=inode_used_percent&range=1h&disk=/&disk=/other"} {
		apiError(t, apiTokenRequest(t, h, "GET", base+query, "Bearer "+token, nil), 400, "validation_failed")
	}
}

func TestAPITokenHTTPRateLimitAndAdminSettings(t *testing.T) {
	f := newHTTP(t, 5)
	metadata, token, cookie, csrf := createHTTPAPIToken(t, f)
	patch := func(path, body string) *httptest.ResponseRecorder {
		return request(t, f.handler, "PATCH", path, body, cookie, csrf, publicURL)
	}
	assertHTTPStatus(t, patch("/api/v1/settings/api-tokens", `{"rate_limit_per_minute":1}`), 200)
	got := request(t, f.handler, "GET", "/api/v1/settings/api-tokens", "", cookie, "", "")
	assertHTTPStatus(t, got, 200)
	if got.Body.String() != "{\"rate_limit_per_minute\":1}\n" {
		t.Fatal(got.Body.String())
	}
	assertHTTPStatus(t, patch("/api/v1/api-tokens/"+metadata.ID, `{"rate_limit_per_minute":1}`), 200)
	// The forbidden request spends the one-request budget before whitelist lookup.
	apiError(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/settings/api-tokens", "Bearer "+token, cookie), 403, "token_forbidden")
	limited := apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", "Bearer "+token, cookie)
	apiError(t, limited, 429, "rate_limited")
	if limited.Header().Get("Retry-After") != "60" {
		t.Fatal("retry", limited.Header())
	}
	var envelope map[string]any
	if err := json.Unmarshal(limited.Body.Bytes(), &envelope); err != nil || envelope["message"] == "" || envelope["request_id"] == "" {
		t.Fatal("envelope", err)
	}
	assertHTTPStatus(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/nodes", "", cookie), 200)
	// The fixture clock is fixed, so a second token covers routes the first can no longer reach.
	assertHTTPStatus(t, patch("/api/v1/settings/api-tokens", `{"rate_limit_per_minute":20}`), 200)
	_, token, cookie, _ = createHTTPAPIToken(t, f)
	for _, endpoint := range []struct{ method, path string }{{"GET", "/api/v1/settings/api-tokens"}, {"PATCH", "/api/v1/settings/api-tokens"}, {"PATCH", "/api/v1/api-tokens/" + metadata.ID}} {
		apiError(t, apiTokenRequest(t, f.handler, endpoint.method, endpoint.path, "Bearer "+token, cookie), 403, "token_forbidden")
	}
	scraper, ok := f.handler.(interface{ MetricsHandler() http.Handler })
	if !ok {
		t.Fatal("metrics handler")
	}
	metrics := httptest.NewRecorder()
	scraper.MetricsHandler().ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), "orbitbeacon_api_token_rate_limited_total 1\n") {
		t.Fatal("counter", metrics.Body.String())
	}
}

func TestAPITokenHTTPRatePatchValidationAuditAndRollback(t *testing.T) {
	f := newHTTP(t, 5)
	metadata, _, cookie, csrf := createHTTPAPIToken(t, f)
	tokenPath := "/api/v1/api-tokens/" + metadata.ID
	patch := func(path, body string) *httptest.ResponseRecorder {
		return request(t, f.handler, "PATCH", path, body, cookie, csrf, publicURL)
	}
	for _, path := range []string{tokenPath, "/api/v1/settings/api-tokens"} {
		for _, body := range []string{`{}`, `{"rate_limit_per_minute":0}`, `{"rate_limit_per_minute":100001}`, `{"rate_limit_per_minute":1.5}`, `{"rate_limit_per_minute":"2"}`, `{"rate_limit_per_minute":true}`, `{"rate_limit_per_minute":2,"password":"never-reflect"}`} {
			response := patch(path, body)
			apiError(t, response, 400, "validation_failed")
			var value struct {
				Fields map[string]string `json:"fields"`
			}
			if json.Unmarshal(response.Body.Bytes(), &value) != nil || len(value.Fields) == 0 {
				t.Fatal("missing fields")
			}
			if body == `{}` && value.Fields["rate_limit_per_minute"] != "is required" {
				t.Fatal("missing field confused with null")
			}
			if strings.Contains(response.Body.String(), "never-reflect") {
				t.Fatal("request value reflected")
			}
		}
	}
	apiError(t, patch("/api/v1/settings/api-tokens", `{"rate_limit_per_minute":null}`), 400, "validation_failed")
	apiError(t, patch("/api/v1/api-tokens/00000000-0000-4000-8000-000000000099", `{"rate_limit_per_minute":2}`), 404, "not_found")
	var count int
	f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action IN ('api_token.rate_limit.update','settings.api_token_rate_limit.update')`).Scan(&count)
	if count != 0 {
		t.Fatal("audit on failure", count)
	}
	for _, item := range []struct{ path, body string }{{tokenPath, `{"rate_limit_per_minute":2}`}, {tokenPath, `{"rate_limit_per_minute":null}`}, {"/api/v1/settings/api-tokens", `{"rate_limit_per_minute":7}`}} {
		assertHTTPStatus(t, patch(item.path, item.body), 200)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action IN ('api_token.rate_limit.update','settings.api_token_rate_limit.update')`).Scan(&count); err != nil || count != 3 {
		t.Fatal("audit", count, err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER rate_http_audit_failure BEFORE INSERT ON audit_events WHEN NEW.action IN ('api_token.rate_limit.update','settings.api_token_rate_limit.update') BEGIN SELECT RAISE(FAIL,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, patch(tokenPath, `{"rate_limit_per_minute":9}`), 500)
	assertHTTPStatus(t, patch("/api/v1/settings/api-tokens", `{"rate_limit_per_minute":9}`), 500)
	var override *int
	var global string
	if err := f.db.QueryRow(`SELECT rate_limit_per_minute FROM api_tokens WHERE id=?`, metadata.ID).Scan(&override); err != nil || override != nil {
		t.Fatal("token rollback", err)
	}
	if err := f.db.QueryRow(`SELECT value FROM settings WHERE key='api_token_rate_limit_per_minute'`).Scan(&global); err != nil || global != "7" {
		t.Fatal("global rollback", global, err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action IN ('api_token.rate_limit.update','settings.api_token_rate_limit.update')`).Scan(&count); err != nil || count != 3 {
		t.Fatal("failed audit persisted", count, err)
	}
	// The same session and CSRF gates as revoke protect both mutation routes.
	for _, path := range []string{tokenPath, "/api/v1/settings/api-tokens"} {
		assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"rate_limit_per_minute":2}`, cookie, "", publicURL), 403)
	}
}
