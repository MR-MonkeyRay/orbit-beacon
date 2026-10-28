package server

import (
	"gopkg.in/yaml.v3"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAPITokenReadPolicyAndQueryCredentials(t *testing.T) {
	for _, path := range []string{"/api/v1/nodes", "/api/v1/nodes/123/config", "/api/v1/nodes/123/traffic", "/api/v1/nodes/123/probes", "/api/v1/audit", "/api/v1/alerts", "/api/v1/alerts/summary", "/api/v1/notification-channels", "/api/v1/notification-channels/123"} {
		if !apiTokenReadAllowed(path) {
			t.Errorf("allowed path denied: %s", path)
		}
	}
	for _, path := range []string{"/api/agent/v1/report", "/api/public/v1/nodes", "/api/v1/me", "/api/v1/me/password", "/api/v1/session", "/api/v1/users", "/api/v1/users/123", "/api/v1/users-future", "/api/v1/api-tokens", "/api/v1/enrollments/123", "/api/v1/notification-channels/123/test"} {
		if apiTokenReadAllowed(path) {
			t.Errorf("sensitive path allowed: %s", path)
		}
	}
	for _, query := range []string{"token=anything", "access_token=secret", "api_token=secret", "Authorization=Bearer", "other=obt_secret", "%74oken=secret", "obt_secret", "access_token=obt_secret;hidden", "%6fbt_secret=1&bad=%ZZ", "x=%4FBT_secret&bad=%zz", "TOKEN=1&bad=%zz"} {
		if !tokenInQuery(httptest.NewRequest("GET", "https://example.test/api/v1/nodes?"+query, nil)) {
			t.Errorf("query token accepted: %s", query)
		}
	}
	if tokenInQuery(httptest.NewRequest("GET", "https://example.test/api/v1/nodes?range=%zz", nil)) {
		t.Fatal("malformed query without a token mistaken for token")
	}
	if tokenInQuery(httptest.NewRequest("GET", "https://example.test/api/v1/nodes?limit=100&offset=0", nil)) {
		t.Fatal("pagination mistaken for token")
	}
}

func TestAPITokenOpenAPISecurityAlternatives(t *testing.T) {
	// Every documented allowed GET must declare both cookie and bearer auth;
	// sensitive GETs must continue to inherit session-only security.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../api/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	schemes := document["components"].(map[string]any)["securitySchemes"].(map[string]any)
	if schemes["bearerAuth"] == nil {
		t.Fatal("bearerAuth scheme missing")
	}
	paths := document["paths"].(map[string]any)
	for path, raw := range paths {
		item := raw.(map[string]any)
		get, ok := item["get"].(map[string]any)
		if !ok || !strings.HasPrefix(path, "/api/v1/") {
			continue
		}
		security, hasSecurity := get["security"].([]any)
		bearer, session := false, false
		for _, entry := range security {
			for key := range entry.(map[string]any) {
				bearer = bearer || key == "bearerAuth"
				session = session || key == "session"
			}
		}
		if apiTokenReadAllowed(path) {
			if !bearer || !session {
				t.Errorf("allowed GET %s missing auth alternative", path)
			}
		} else if hasSecurity && bearer {
			t.Errorf("sensitive GET %s allows bearer", path)
		}
	}
	for _, path := range []string{"/api/v1/api-tokens", "/api/v1/api-tokens/{id}"} {
		if paths[path] == nil {
			t.Errorf("missing token management contract %s", path)
		}
	}
}
