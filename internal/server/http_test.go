package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

const publicURL = "https://beacon.example.test"
const password = "HTTP regression password"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

type fixture struct {
	handler http.Handler
	auth    *auth.Service
	nodes   *nodes.Service
	db      *sql.DB
}

func newHTTP(t *testing.T, capacity int) fixture {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }
	authService := auth.NewService(db, auth.Options{Now: now, Random: rand.NewChaCha8([32]byte{31})})
	nodeService := nodes.NewService(db, nodes.Options{MaxNodes: capacity, Now: now, Random: rand.NewChaCha8([32]byte{32})})
	if _, err := authService.BootstrapAdministrator(context.Background(), "admin", password); err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateUser(context.Background(), "viewer", password, auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(server.Options{
		Auth: authService, Nodes: nodeService, PublicURL: publicURL,
		Assets: fstest.MapFS{
			"200.html":               &fstest.MapFile{Data: []byte("<!doctype html><main>regression SPA shell</main>")},
			"login.html":             &fstest.MapFile{Data: []byte("<!doctype html><main>regression login shell</main>")},
			"_app/immutable/main.js": &fstest.MapFile{Data: []byte("export const fixture = true;")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{handler: handler, auth: authService, nodes: nodeService, db: db}
}

func request(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, publicURL+path, strings.NewReader(body))
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
	handler.ServeHTTP(response, req)
	return response
}

func login(t *testing.T, f fixture, username string) (*http.Cookie, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodPost, "/api/v1/session", string(body), nil, "", publicURL)
	if response.Code != http.StatusOK {
		t.Fatalf("login returned %d: %s", response.Code, response.Body)
	}
	var result struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.CSRFToken == "" {
		t.Fatal("login did not return a CSRF token")
	}
	cookies := response.Result().Cookies()
	var session *http.Cookie
	for _, cookie := range cookies {
		if cookie.Value != "" && cookie.HttpOnly {
			session = cookie
			break
		}
	}
	if session == nil {
		t.Fatal("login did not set an HttpOnly session cookie")
	}
	if !session.Secure || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Fatalf("unsafe session cookie attributes: %+v", session)
	}
	wantExpiry := time.Date(2026, 3, 4, 17, 6, 7, 0, time.UTC)
	if session.MaxAge != 12*60*60 && !session.Expires.Equal(wantExpiry) {
		t.Errorf("session cookie does not expire after 12 hours: MaxAge=%d Expires=%s", session.MaxAge, session.Expires)
	}
	return session, result.CSRFToken
}

func apiError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) map[string]json.RawMessage {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP status = %d, want %d: %s", response.Code, status, response.Body)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("API error Content-Type = %q", contentType)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatalf("error is not a JSON object: %v", err)
	}
	var actualCode, message, requestID string
	if err := json.Unmarshal(object["code"], &actualCode); err != nil {
		t.Fatal(err)
	}
	if actualCode != code {
		t.Errorf("error code = %q, want %q", actualCode, code)
	}
	if err := json.Unmarshal(object["message"], &message); err != nil || message == "" {
		t.Errorf("error has no human-readable message: %v", err)
	}
	if err := json.Unmarshal(object["request_id"], &requestID); err != nil || !uuidPattern.MatchString(requestID) {
		t.Errorf("error request_id is not a UUID: %q, %v", requestID, err)
	}
	for key := range object {
		if key != "code" && key != "message" && key != "request_id" && key != "fields" {
			t.Errorf("unexpected API error member %q", key)
		}
	}
	return object
}

func TestRoleAuthorizationProtectsMutationsAndSecrets(t *testing.T) {
	f := newHTTP(t, 5)
	cookie, csrf := login(t, f, "viewer")
	for _, path := range []string{"/api/v1/me", "/api/v1/nodes"} {
		response := request(t, f.handler, http.MethodGet, path, "", cookie, "", "")
		if response.Code != http.StatusOK {
			t.Errorf("viewer cannot read %s: %d", path, response.Code)
		}
	}
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "private node"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/nodes", `{"name":"not allowed"}`},
		{http.MethodPost, "/api/v1/users", `{"username":"intruder","password":"not allowed","role":"admin"}`},
		{http.MethodPost, "/api/v1/enrollments", `{"node_id":"` + node.ID + `"}`},
		{http.MethodPatch, "/api/v1/nodes/" + node.ID + "/config", `{"report_interval_seconds":30}`},
		{http.MethodPost, "/api/v1/nodes/batch", `{"node_ids":["` + node.ID + `"],"action":"disable"}`},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			response := request(t, f.handler, tt.method, tt.path, tt.body, cookie, csrf, publicURL)
			apiError(t, response, http.StatusForbidden, "forbidden")
		})
	}
	response := request(t, f.handler, http.MethodGet, "/api/v1/nodes/"+node.ID, "", cookie, "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("viewer node read = %d", response.Code)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"token", "node_token", "token_hash", "password_hash", "enrollment_token"} {
		if _, exists := object[key]; exists {
			t.Errorf("private read exposed credential member %q", key)
		}
	}
}

func TestMutationsRequireBothTrustedOriginAndSessionBoundCSRF(t *testing.T) {
	f := newHTTP(t, 20)
	cookie, csrf := login(t, f, "admin")
	_, otherCSRF := login(t, f, "viewer")
	for _, tt := range []struct {
		name   string
		origin string
		csrf   string
	}{
		{"missing Origin", "", csrf},
		{"opaque Origin", "null", csrf},
		{"foreign Origin", "https://attacker.example.test", csrf},
		{"same host wrong scheme", "http://beacon.example.test", csrf},
		{"same host wrong port", "https://beacon.example.test:8443", csrf},
		{"missing CSRF", publicURL, ""},
		{"wrong CSRF", publicURL, "not-a-valid-token"},
		{"other session CSRF", publicURL, otherCSRF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"must not exist"}`, cookie, tt.csrf, tt.origin)
			if response.Code != http.StatusForbidden {
				t.Fatalf("untrusted mutation returned %d: %s", response.Code, response.Body)
			}
		})
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("rejected mutations created %d nodes", count)
	}
	response := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"allowed"}`, cookie, csrf, publicURL)
	if response.Code != http.StatusCreated {
		t.Fatalf("valid mutation rejected: %d %s", response.Code, response.Body)
	}
	// A client-controlled Host cannot redefine which Origin is trusted.
	req := httptest.NewRequest(http.MethodPost, "https://attacker.example.test/api/v1/nodes", strings.NewReader(`{"name":"host spoof"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://attacker.example.test")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	response = httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Errorf("Host header changed trusted Origin: %d", response.Code)
	}
}

func TestNodeWireFormatAndCapacityError(t *testing.T) {
	f := newHTTP(t, 1)
	cookie, csrf := login(t, f, "admin")
	response := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"first node"}`, cookie, csrf, publicURL)
	if response.Code != http.StatusCreated {
		t.Fatalf("node creation returned %d: %s", response.Code, response.Body)
	}
	var node struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	if !uuidPattern.MatchString(node.ID) || node.Name != "first node" || node.Status != "pending" {
		t.Errorf("node wire identity = %+v", node)
	}
	createdAt, err := time.Parse(time.RFC3339, node.CreatedAt)
	if err != nil || !strings.HasSuffix(node.CreatedAt, "Z") || !createdAt.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) {
		t.Errorf("created_at must be UTC RFC3339 from the injected clock: %q, %v", node.CreatedAt, err)
	}
	response = request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"too many"}`, cookie, csrf, publicURL)
	apiError(t, response, http.StatusConflict, "node_capacity_reached")
}

func TestInvalidDesiredConfigReportsEveryInvalidFieldWithoutChangingRevision(t *testing.T) {
	f := newHTTP(t, 2)
	cookie, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "config target"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	before := request(t, f.handler, http.MethodGet, path, "", cookie, "", "")
	if before.Code != http.StatusOK {
		t.Fatalf("read desired configuration = %d: %s", before.Code, before.Body)
	}
	response := request(t, f.handler, http.MethodPatch, path,
		`{"report_interval_seconds":4,"disk_interval_seconds":59,"billing_day":29}`,
		cookie, csrf, publicURL)
	object := apiError(t, response, http.StatusBadRequest, "validation_failed")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object["fields"], &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"report_interval_seconds", "disk_interval_seconds", "billing_day"} {
		if value, ok := fields[key]; !ok || len(value) == 0 || bytes.Equal(value, []byte(`""`)) {
			t.Errorf("missing field error for %s", key)
		}
	}
	after := request(t, f.handler, http.MethodGet, path, "", cookie, "", "")
	if after.Code != http.StatusOK {
		t.Fatalf("reading configuration after rejected update = %d: %s", after.Code, after.Body)
	}
	var beforeConfig, afterConfig map[string]any
	if err := json.Unmarshal(before.Body.Bytes(), &beforeConfig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after.Body.Bytes(), &afterConfig); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"revision", "report_interval_seconds", "disk_interval_seconds", "billing_day"} {
		if beforeConfig[key] == nil || beforeConfig[key] != afterConfig[key] {
			t.Errorf("rejected configuration changed %s: before=%v after=%v", key, beforeConfig[key], afterConfig[key])
		}
	}
}

func TestAPIFailuresHaveStructuredErrorsAndNeverSPAFallback(t *testing.T) {
	f := newHTTP(t, 2)
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", nil, "", ""), http.StatusUnauthorized, "unauthenticated")
	cookie, csrf := login(t, f, "admin")
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/not-a-route", "", cookie, "", ""), http.StatusNotFound, "not_found")
	apiError(t, request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{`, cookie, csrf, publicURL), http.StatusBadRequest, "invalid_json")
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000999", "", cookie, "", ""), http.StatusNotFound, "node_not_found")
	response := request(t, f.handler, http.MethodPut, "/api/v1/nodes", `{}`, cookie, csrf, publicURL)
	apiError(t, response, http.StatusMethodNotAllowed, "method_not_allowed")
	if response.Header().Get("Allow") == "" {
		t.Error("405 response does not declare allowed methods")
	}
	// There is deliberately no anonymous first-administrator registration route.
	response = request(t, f.handler, http.MethodPost, "/api/v1/bootstrap", `{"username":"attacker","password":"takeover"}`, nil, "", publicURL)
	if response.Code != http.StatusNotFound {
		t.Errorf("public bootstrap route returned %d, want 404", response.Code)
	}
}

func TestStaticFallbackIsLimitedToDeclaredUIRoutes(t *testing.T) {
	f := newHTTP(t, 2)
	cookie, _ := login(t, f, "admin")
	for _, path := range []string{"/login", "/overview", "/nodes", "/nodes/00000000-0000-4000-8000-000000000123", "/alerts", "/settings", "/status"} {
		t.Run(path, func(t *testing.T) {
			response := request(t, f.handler, http.MethodGet, path, "", cookie, "", "")
			if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
				t.Errorf("declared SPA route returned %d, type=%q", response.Code, response.Header().Get("Content-Type"))
			}
			if !bytes.Contains(response.Body.Bytes(), []byte("regression SPA shell")) && !bytes.Contains(response.Body.Bytes(), []byte("regression login shell")) {
				t.Error("declared route did not serve a UI document")
			}
		})
	}
	for _, path := range []string{
		"/not-a-page", "/overview/unexpected", "/nodes/00000000-0000-4000-8000-000000000123/extra",
		"/_app/immutable/missing.js", "/favicon-missing.svg", "/api/v1/missing", "/api/agent/v1/missing",
	} {
		t.Run(path, func(t *testing.T) {
			response := request(t, f.handler, http.MethodGet, path, "", cookie, "", "")
			if response.Code != http.StatusNotFound {
				t.Errorf("unknown path returned %d, want a real 404", response.Code)
			}
			if bytes.Contains(response.Body.Bytes(), []byte("regression SPA shell")) {
				t.Error("unknown path was served the SPA shell")
			}
		})
	}
	response := request(t, f.handler, http.MethodGet, "/_app/immutable/main.js", "", nil, "", "")
	if response.Code != http.StatusOK || response.Body.String() != "export const fixture = true;" {
		t.Errorf("real static asset not delivered: %d %s", response.Code, response.Body)
	}
	response = request(t, f.handler, http.MethodHead, "/_app/immutable/main.js", "", nil, "", "")
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Errorf("HEAD asset response = %d with %d body bytes", response.Code, response.Body.Len())
	}
}

func TestRootRedirectUsesSession(t *testing.T) {
	f := newHTTP(t, 2)
	cookie, _ := login(t, f, "admin")
	for _, tt := range []struct {
		cookie   *http.Cookie
		location string
	}{{nil, "/login"}, {cookie, "/overview"}} {
		response := request(t, f.handler, http.MethodGet, "/", "", tt.cookie, "", "")
		if response.Code < 300 || response.Code >= 400 || response.Header().Get("Location") != tt.location {
			t.Errorf("root redirect = %d %q, want %q", response.Code, response.Header().Get("Location"), tt.location)
		}
	}
}

func TestLogoutRevokesCookieSession(t *testing.T) {
	f := newHTTP(t, 2)
	cookie, csrf := login(t, f, "admin")
	response := request(t, f.handler, http.MethodDelete, "/api/v1/session", "", cookie, csrf, publicURL)
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout returned %d: %s", response.Code, response.Body)
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestBatchHTTPReturnsPerNodeResultsAndRejectsImplicitSelection(t *testing.T) {
	f := newHTTP(t, 3)
	cookie, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "batch target"})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodPost, "/api/v1/nodes/batch", `{"action":"disable"}`, cookie, csrf, publicURL)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("implicit batch selection returned %d: %s", response.Code, response.Body)
	}
	const missing = "00000000-0000-4000-8000-000000000999"
	body, err := json.Marshal(map[string]any{"node_ids": []string{node.ID, missing}, "action": "disable"})
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, f.handler, http.MethodPost, "/api/v1/nodes/batch", string(body), cookie, csrf, publicURL)
	if response.Code != http.StatusOK {
		t.Fatalf("batch returned %d: %s", response.Code, response.Body)
	}
	var result struct {
		Results []struct {
			NodeID    string `json:"node_id"`
			Status    string `json:"status"`
			ErrorCode string `json:"error_code"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("batch must return a result for every ID, got %d", len(result.Results))
	}
	seen := make(map[string]bool)
	for _, item := range result.Results {
		if seen[item.NodeID] {
			t.Fatalf("duplicate result for %s", item.NodeID)
		}
		seen[item.NodeID] = true
		switch item.NodeID {
		case node.ID:
			if item.Status != "succeeded" || item.ErrorCode != "" {
				t.Errorf("successful result = %+v", item)
			}
		case missing:
			if item.Status != "failed" || item.ErrorCode != "node_not_found" {
				t.Errorf("missing node result = %+v", item)
			}
		default:
			t.Errorf("result refers to an unrequested node: %+v", item)
		}
	}
}
