package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestInitialPasswordRestrictionAndTransition(t *testing.T) {
	agent := newAgentHTTP(t, 5)
	_, err := agent.auth.BootstrapAdministrator(context.Background(), "initial-admin", password)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{handler: agent.handler, auth: agent.auth, nodes: agent.nodes, db: agent.db}
	first, csrf := login(t, f, "initial-admin")
	second, _ := login(t, f, "initial-admin")
	me := request(t, f.handler, http.MethodGet, "/api/v1/me", "", first, "", "")
	assertHTTPStatus(t, me, 200)
	user := decodeObject(t, me)["user"].(map[string]any)
	if user["must_change_password"] != true {
		t.Fatalf("initial session not restricted: %+v", user)
	}
	id := "00000000-0000-4000-8000-000000000001"
	for _, path := range []string{"/api/v1/nodes", "/api/v1/users", "/api/v1/notifications", "/api/v1/notifications/summary", "/api/v1/tasks", "/api/v1/tasks/" + id + "/download", "/api/v1/backup-plans", "/api/v1/live", "/api/v1/live/stream", "/api/v1/live/ws", "/api/v1/audit", "/api/v1/alert-rules"} {
		apiError(t, request(t, f.handler, http.MethodGet, path, "", first, "", ""), 403, "password_change_required")
	}
	apiError(t, request(t, f.handler, http.MethodPost, "/api/v1/tasks/"+id+"/password", `{}`, first, csrf, publicURL), 403, "password_change_required")
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/unknown", "", first, "", ""), 404, "not_found")
	apiError(t, request(t, f.handler, http.MethodPost, "/api/v1/me", `{}`, first, csrf, publicURL), 405, "method_not_allowed")
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/", "", first, "", ""), 200)
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/public/v1/nodes", "", first, "", ""), 200)
	logoutCookie, logoutCSRF := login(t, f, "initial-admin")
	apiError(t, request(t, f.handler, http.MethodDelete, "/api/v1/session", "", logoutCookie, logoutCSRF, ""), 403, "forbidden")
	assertHTTPStatus(t, request(t, f.handler, http.MethodDelete, "/api/v1/session", "", logoutCookie, logoutCSRF, publicURL), 204)
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", logoutCookie, "", ""), 401, "unauthenticated")
	change := fmt.Sprintf(`{"old_password":%q,"new_password":"changed-admin-password"}`, password)
	apiError(t, request(t, f.handler, http.MethodPatch, "/api/v1/me/password", change, first, csrf, ""), 403, "forbidden")
	apiError(t, request(t, f.handler, http.MethodPatch, "/api/v1/me/password", change, first, "", publicURL), 403, "forbidden")
	apiError(t, request(t, f.handler, http.MethodPatch, "/api/v1/me/password", fmt.Sprintf(`{"old_password":%q,"new_password":%q}`, password, password), first, csrf, publicURL), 400, "password_unchanged")
	apiError(t, request(t, f.handler, http.MethodPatch, "/api/v1/me/password", `{"old_password":"wrong-password","new_password":"changed-admin-password"}`, first, csrf, publicURL), 400, "invalid_old_password")
	apiError(t, request(t, f.handler, http.MethodPatch, "/api/v1/me/password", fmt.Sprintf(`{"old_password":%q,"new_password":%q}`, password, strings.Repeat("x", 1025)), first, csrf, publicURL), 400, "validation_failed")
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", first, "", ""), 200)
	changed := request(t, f.handler, http.MethodPatch, "/api/v1/me/password", change, first, csrf, publicURL)
	assertHTTPStatus(t, changed, 204)
	if len(changed.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("password response can clear a newer login cookie")
	}
	for _, cookie := range []*http.Cookie{first, second} {
		apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", ""), 401, "unauthenticated")
	}
	apiError(t, request(t, f.handler, http.MethodPost, "/api/v1/session", fmt.Sprintf(`{"username":"initial-admin","password":%q}`, password), nil, "", publicURL), 401, "unauthenticated")
	res := request(t, f.handler, http.MethodPost, "/api/v1/session", `{"username":"initial-admin","password":"changed-admin-password"}`, nil, "", publicURL)
	assertHTTPStatus(t, res, 200)
	if decodeObject(t, res)["user"].(map[string]any)["must_change_password"] != false {
		t.Fatal("changed user remains restricted")
	}
}

func TestCreateAdministratorPasswordChangeOption(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin-two")
	for index, raw := range []string{"true", "false", "", "null", `"true"`, "1"} {
		suffix := ""
		if raw != "" {
			suffix = `,"must_change_password":` + raw
		}
		res := request(t, f.handler, http.MethodPost, "/api/v1/users", fmt.Sprintf(`{"username":"option-%d","password":%q%s}`, index, password, suffix), admin, csrf, publicURL)
		if raw == "null" || raw == `"true"` || raw == "1" {
			assertHTTPStatus(t, res, 400)
			continue
		}
		assertHTTPStatus(t, res, 201)
		created := decodeObject(t, res)
		if created["must_change_password"] != (raw == "true") {
			t.Fatalf("option %s not preserved: %+v", raw, created)
		}
		cookie, _ := login(t, f, fmt.Sprintf("option-%d", index))
		result := request(t, f.handler, http.MethodGet, "/api/v1/nodes", "", cookie, "", "")
		if raw == "true" {
			apiError(t, result, 403, "password_change_required")
		} else {
			assertHTTPStatus(t, result, 200)
		}
		assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, "/api/v1/users/"+created["id"].(string), `{"must_change_password":false}`, admin, csrf, publicURL), 400)
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM users WHERE username LIKE 'option-%'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("invalid option created user: count=%d err=%v", count, err)
	}
}
