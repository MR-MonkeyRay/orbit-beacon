package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var literalTokenRoutes = []string{
	"GET /api/v1/service-groups",
	"GET /api/v1/services",
	"GET /api/v1/services/{id}",
	"GET /api/v1/services/{id}/history",
	"GET /api/v1/nodes",
	"GET /api/v1/nodes/{id}",
	"GET /api/v1/nodes/{id}/latest",
	"GET /api/v1/nodes/{id}/history",
	"GET /api/v1/nodes/{id}/availability",
	"GET /api/v1/nodes/{id}/traffic",
	"GET /api/v1/nodes/{id}/probes",
	"GET /api/v1/alerts",
	"GET /api/v1/alerts/summary",
	"GET /api/v1/alerts/{id}",
	"GET /api/v1/groups",
}

func TestAPITokenRouteAllowlistAndOpenAPIMarkers(t *testing.T) {
	actual := make([]string, 0, len(tokenRoutes))
	for pattern := range tokenRoutes {
		actual = append(actual, pattern)
	}
	expected := slices.Clone(literalTokenRoutes)
	slices.Sort(actual)
	slices.Sort(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("tokenRoutes=%v want %v", actual, expected)
	}
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	marked := []string{}
	for path, operations := range doc.Paths {
		for method, operation := range operations {
			var entry struct {
				Token bool `yaml:"x-read-only-token"`
			}
			if openAPIMethodNames[method] {
				if err := operation.Decode(&entry); err != nil {
					t.Fatal(err)
				}
			}
			if entry.Token {
				marked = append(marked, strings.ToUpper(method)+" "+path)
			}
		}
	}
	slices.Sort(marked)
	if !reflect.DeepEqual(marked, expected) {
		t.Fatalf("OpenAPI markers=%v", marked)
	}
}

func TestAPITokenProbeHistoryForbidden(t *testing.T) {
	h := openAPITestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "https://beacon.example.test/api/v1/nodes/00000000-0000-4000-8000-000000000001/probes/history?target=https%3A%2F%2Fexample.test&range=24h", nil)
	ctx := context.Background()
	user, err := h.auth.CreateUser(ctx, "history-token-owner", "token regression password", false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.auth.Login(ctx, user.Username, "token regression password", "")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := h.auth.CreateAPIToken(ctx, user.ID, session.Token, "probe-history", nil, "token regression password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "obt_") {
		t.Fatal("unexpected token prefix")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusForbidden || body.Code != "token_forbidden" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
}

func TestAPITokenAllManagementRoutesDefaultDeny(t *testing.T) {
	h := openAPITestHandler(t)
	ctx := context.Background()
	user, err := h.auth.CreateUser(ctx, "token-owner", "token regression password", false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.auth.Login(ctx, user.Username, "token regression password", "")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := h.auth.CreateAPIToken(ctx, user.ID, session.Token, "route-sweep", nil, "token regression password")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range routes {
		if entry.auth != admin && entry.auth != pendingPasswordAllowed {
			continue
		}
		if slices.Contains(literalTokenRoutes, entry.pattern) {
			continue
		}
		t.Run(entry.pattern, func(t *testing.T) {
			verb, path, _ := strings.Cut(entry.pattern, " ")
			path = strings.ReplaceAll(path, "{id}", "00000000-0000-4000-8000-000000000001")
			path = strings.ReplaceAll(path, "{command_id}", "00000000-0000-4000-8000-000000000002")
			req := httptest.NewRequest(verb, "https://beacon.example.test"+path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
			req.Header.Set("Origin", "https://beacon.example.test")
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, req)
			var body struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			if response.Code != 403 || body.Code != "token_forbidden" {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}
