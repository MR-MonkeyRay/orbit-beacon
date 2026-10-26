package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"orbitbeacon/internal/server/auth"
)

func TestCurrentSessionRestoresStableCSRFAcrossTabs(t *testing.T) {
	f := newHTTP(t, 5)
	cookie, originalCSRF := login(t, f, "admin")
	var recovered auth.Session
	for range 2 {
		response := request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", "")
		assertHTTPStatus(t, response, http.StatusOK)
		if err := json.Unmarshal(response.Body.Bytes(), &recovered); err != nil {
			t.Fatal(err)
		}
		if recovered.User.Username != "admin" || recovered.CSRFToken != originalCSRF || recovered.ExpiresAt.IsZero() {
			t.Fatalf("active session cannot recover its stable CSRF: %+v", recovered)
		}
		if strings.Contains(response.Body.String(), cookie.Value) {
			t.Fatal("CSRF recovery exposed the HttpOnly session credential")
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("CSRF recovery can be cached")
		}
	}
	otherCookie, otherCSRF := login(t, f, "admin")
	apiError(t, request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"cross-session"}`, cookie, otherCSRF, publicURL), http.StatusForbidden, "forbidden")
	assertHTTPStatus(t, request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"recovered-tab"}`, cookie, recovered.CSRFToken, publicURL), http.StatusCreated)
	assertHTTPStatus(t, request(t, f.handler, http.MethodDelete, "/api/v1/session", "", cookie, recovered.CSRFToken, publicURL), http.StatusNoContent)
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", ""), http.StatusUnauthorized, "unauthenticated")
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", otherCookie, "", ""), http.StatusOK)
}
