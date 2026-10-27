package server_test

import (
	"net/http"
	"testing"
)

func TestPublicRouteIsExactAndMethodCheckedBeforeAuthentication(t *testing.T) {
	f := newHTTP(t, 2)
	response := request(t, f.handler, http.MethodGet, "/api/public/v1/nodes", "", nil, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public response can be cached")
	}
	response = request(t, f.handler, http.MethodPost, "/api/public/v1/nodes", "{}", nil, "", "")
	assertHTTPStatus(t, response, http.StatusMethodNotAllowed)
	if response.Header().Get("Allow") != "GET" {
		t.Fatalf("Allow: %q", response.Header().Get("Allow"))
	}
	for _, path := range []string{"/api/public/v1/nodes/extra", "/api/public/v1/unknown", "/api/public/"} {
		assertHTTPStatus(t, request(t, f.handler, http.MethodGet, path, "", nil, "", ""), http.StatusNotFound)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes", "", nil, "", ""), http.StatusUnauthorized)
}
