package server_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/timekey"
)

func bearerRequest(t *testing.T, f fixture, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, publicURL+path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:1234"
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("token response set cookies")
	}
	return response
}

func createHTTPToken(t *testing.T, f fixture) auth.CreatedAPIToken {
	t.Helper()
	cookie, csrf := login(t, f, "admin")
	response := request(t, f.handler, "POST", "/api/v1/api-tokens", `{"name":"monitor","expires_in_days":30}`, cookie, csrf, publicURL)
	if response.Code != 201 {
		t.Fatalf("create %d %s", response.Code, response.Body)
	}
	var token auth.CreatedAPIToken
	if err := json.Unmarshal(response.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAPITokenHTTPReadOnlyAndExcludedPaths(t *testing.T) {
	f := newHTTP(t, 2)
	token := createHTTPToken(t, f)
	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{"/api/v1/nodes", "/api/v1/groups", "/api/v1/audit", "/api/v1/alerts", "/api/v1/alerts/summary", "/api/v1/alert-rules"} {
			response := bearerRequest(t, f, method, path, token.Token)
			if response.Code != 200 {
				t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body)
			}
			if method == "HEAD" && response.Body.Len() != 0 {
				t.Fatal("HEAD returned a body")
			}
		}
	}
	for _, path := range []string{"/api/v1/me", "/api/v1/me/password", "/api/v1/session", "/api/v1/users", "/api/v1/users/00000000-0000-4000-8000-000000000001", "/api/v1/enrollments", "/api/v1/api-tokens", "/api/v1/api-tokens/00000000-0000-4000-8000-000000000001", "/api/v1/notification-channels/00000000-0000-4000-8000-000000000001/test"} {
		response := bearerRequest(t, f, "GET", path, token.Token)
		if response.Code != 403 || !strings.Contains(response.Body.String(), "token_forbidden") {
			t.Fatalf("excluded %s: %d %s", path, response.Code, response.Body)
		}
	}
	for _, method := range []string{"POST", "PATCH", "PUT", "DELETE", "OPTIONS"} {
		response := bearerRequest(t, f, method, "/api/v1/nodes", token.Token)
		if response.Code != 403 || !strings.Contains(response.Body.String(), "token_read_only") {
			t.Fatalf("write %s: %d %s", method, response.Code, response.Body)
		}
	}
	cookie, csrf := login(t, f, "admin")
	req := httptest.NewRequest("POST", publicURL+"/api/v1/nodes", strings.NewReader(`{}`))
	req.AddCookie(cookie)
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Origin", publicURL)
	req.Header.Set("X-CSRF-Token", csrf)
	res := httptest.NewRecorder()
	f.handler.ServeHTTP(res, req)
	if res.Code != 403 || !strings.Contains(res.Body.String(), "token_read_only") {
		t.Fatal("session elevated bearer permissions")
	}
}

func TestAPITokenHTTPQueryExpiryRevocationAndManagement(t *testing.T) {
	f := newHTTP(t, 1)
	token := createHTTPToken(t, f)
	for _, query := range []string{"?token=" + token.Token, "?access_token=" + token.Token, "?api_token=" + token.Token, "?anything=" + token.Token} {
		response := bearerRequest(t, f, "GET", "/api/v1/nodes"+query, token.Token)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "token_in_query") {
			t.Fatalf("query: %d %s", response.Code, response.Body)
		}
	}
	cookie, csrf := login(t, f, "admin")
	listed := request(t, f.handler, "GET", "/api/v1/api-tokens", "", cookie, "", "")
	if listed.Code != 200 || strings.Contains(listed.Body.String(), token.Token) || strings.Contains(listed.Body.String(), "token_hash") {
		t.Fatalf("list %d %s", listed.Code, listed.Body)
	}
	viewer, viewerCSRF := login(t, f, "viewer")
	for _, method := range []string{"GET", "POST", "DELETE"} {
		path := "/api/v1/api-tokens"
		if method == "DELETE" {
			path += "/" + token.ID
		}
		response := request(t, f.handler, method, path, `{"name":"denied"}`, viewer, viewerCSRF, publicURL)
		if response.Code != 403 {
			t.Fatalf("viewer %s: %d", method, response.Code)
		}
	}
	for _, credentials := range []struct{ csrf, origin string }{{"", publicURL}, {csrf, ""}} {
		response := request(t, f.handler, "POST", "/api/v1/api-tokens", `{"name":"denied"}`, cookie, credentials.csrf, credentials.origin)
		if response.Code != 403 {
			t.Fatalf("missing CSRF/Origin: %d", response.Code)
		}
	}
	if _, err := f.db.Exec(`UPDATE api_tokens SET expires_at=? WHERE id=?`, timekey.Format(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), token.ID); err != nil {
		t.Fatal(err)
	}
	response := bearerRequest(t, f, "GET", "/api/v1/nodes", token.Token)
	if response.Code != 401 || !strings.Contains(response.Body.String(), "invalid_token") {
		t.Fatalf("expired: %d %s", response.Code, response.Body)
	}
	if _, err := f.db.Exec(`UPDATE api_tokens SET expires_at=NULL WHERE id=?`, token.ID); err != nil {
		t.Fatal(err)
	}
	if response = bearerRequest(t, f, "GET", "/api/v1/nodes", token.Token); response.Code != 200 {
		t.Fatal("restored token failed")
	}
	for range 2 {
		response = request(t, f.handler, "DELETE", "/api/v1/api-tokens/"+token.ID, "", cookie, csrf, publicURL)
		if response.Code != 204 {
			t.Fatalf("revoke: %d %s", response.Code, response.Body)
		}
	}
	response = bearerRequest(t, f, "GET", "/api/v1/nodes", token.Token)
	if response.Code != 401 || !strings.Contains(response.Body.String(), "invalid_token") {
		t.Fatalf("revoked: %d %s", response.Code, response.Body)
	}
}

func TestAPITokenHTTPRateLimitHeadersAndInvalidBearerPrecedence(t *testing.T) {
	f := newHTTP(t, 1)
	token := createHTTPToken(t, f)
	for range 30 {
		response := bearerRequest(t, f, "GET", "/api/v1/nodes", token.Token)
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
	}
	response := bearerRequest(t, f, "GET", "/api/v1/nodes", token.Token)
	if response.Code != 429 || response.Header().Get("Retry-After") == "" {
		t.Fatalf("token rate: %d", response.Code)
	}
	for range 30 {
		response = bearerRequest(t, f, "GET", "/api/v1/nodes", "obt_invalid")
		if response.Code != 401 {
			t.Fatalf("invalid %d", response.Code)
		}
	}
	response = bearerRequest(t, f, "GET", "/api/v1/nodes", "obt_invalid")
	if response.Code != 429 || response.Header().Get("Retry-After") == "" || strings.Contains(response.Body.String(), "Password") {
		t.Fatalf("invalid rate: %d %s", response.Code, response.Body)
	}
	second := createHTTPToken(t, f)
	if response = bearerRequest(t, f, "GET", "/api/v1/nodes", second.Token); response.Code != 200 {
		t.Fatalf("invalid peer budget blocked valid token: %d", response.Code)
	}
	cookie, _ := login(t, f, "admin")
	for _, header := range []string{"Basic not-a-token", "Bearer obt_invalid", "Bearer", ""} {
		req := httptest.NewRequest("GET", publicURL+"/api/v1/nodes", nil)
		req.AddCookie(cookie)
		req.Header["Authorization"] = []string{header}
		res := httptest.NewRecorder()
		f.handler.ServeHTTP(res, req)
		if res.Code != 401 {
			t.Fatalf("invalid header %q fell back to cookie: %d", header, res.Code)
		}
	}
}

func TestAPITokenHTTPNotificationReadsStaySecretFree(t *testing.T) {
	f := newHTTP(t, 1)
	box, err := secretbox.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	notifications := notify.New(f.db, box, notify.Options{})
	h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Notifications: notifications, PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	cookie, csrf := login(t, f, "admin")
	channel := request(t, f.handler, "POST", "/api/v1/notification-channels", validChannel, cookie, csrf, publicURL)
	if channel.Code != 201 {
		t.Fatalf("channel: %d %s", channel.Code, channel.Body)
	}
	id := objectID(t, decodeObject(t, channel))
	token := createHTTPToken(t, f)
	for _, path := range []string{"/api/v1/notification-channels", "/api/v1/notification-channels/" + id, "/api/v1/notification-deliveries"} {
		response := bearerRequest(t, f, "GET", path, token.Token)
		if response.Code != 200 || strings.Contains(response.Body.String(), "NEVER-EXPOSE") || strings.Contains(response.Body.String(), "/notify") {
			t.Fatalf("read %s: %d %s", path, response.Code, response.Body)
		}
	}
	for _, method := range []string{"POST", "GET", "HEAD"} {
		response := bearerRequest(t, f, method, "/api/v1/notification-channels/"+id+"/test", token.Token)
		if response.Code != 403 {
			t.Fatalf("test route %s: %d", method, response.Code)
		}
	}
}
