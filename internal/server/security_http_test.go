package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server"
)

func TestBrowserCanonicalOriginInteroperatesOrConfigurationIsRejected(t *testing.T) {
	f := newHTTP(t, 5)
	for _, tt := range []struct {
		name       string
		configured string
		browser    string
	}{
		{"uppercase DNS", "https://BEACON.EXAMPLE.TEST", publicURL},
		{"default HTTPS port", publicURL + ":443", publicURL},
		{"uppercase DNS and default port", "https://BEACON.EXAMPLE.TEST:443", publicURL},
		{"expanded IPv6", "https://[2001:0db8:0000:0000:0000:0000:0000:0001]:443", "https://[2001:db8::1]"},
		{"uppercase IPv6", "https://[2001:DB8::ABCD]", "https://[2001:db8::abcd]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, configErr := config.LoadServer(config.LoadOptions{
				LookupEnv: func(string) (string, bool) { return "", false },
				Flags:     map[string]string{"public_url": tt.configured},
			})
			handler, handlerErr := server.NewHandler(server.Options{
				Auth: f.auth, Nodes: f.nodes, PublicURL: tt.configured,
				Assets: fstest.MapFS{"200.html": &fstest.MapFile{Data: []byte("<!doctype html><main>shell</main>")}, "public.html": &fstest.MapFile{Data: []byte("public shell")}},
			})
			if configErr != nil || handlerErr != nil {
				if configErr == nil || handlerErr == nil {
					t.Errorf("noncanonical URL rejection inconsistent: config=%v handler=%v", configErr, handlerErr)
				}
				return
			}
			response := request(t, handler, http.MethodPost, "/api/v1/session", `{"username":"admin","password":"`+password+`"}`, nil, "", tt.browser)
			if response.Code != http.StatusOK {
				t.Fatalf("accepted public URL cannot authenticate its canonical browser Origin: %d %s", response.Code, response.Body)
			}
			var result struct {
				CSRFToken string `json:"csrf_token"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			var cookie *http.Cookie
			for _, candidate := range response.Result().Cookies() {
				if candidate.HttpOnly && candidate.Value != "" {
					cookie = candidate
				}
			}
			if cookie == nil || result.CSRFToken == "" {
				t.Fatal("login did not establish a session")
			}
			response = request(t, handler, http.MethodPost, "/api/v1/nodes", `{"name":"canonical origin node"}`, cookie, result.CSRFToken, tt.browser)
			if response.Code != http.StatusCreated {
				t.Errorf("canonical browser Origin cannot mutate: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestUnsupportedMethodsPrecedeAuthenticationAuthorizationAndCSRF(t *testing.T) {
	f := newHTTP(t, 5)
	admin, adminCSRF := login(t, f, "admin")
	viewer, viewerCSRF := login(t, f, "viewer")
	const id = "00000000-0000-4000-8000-000000000123"
	for _, route := range []struct {
		path  string
		allow []string
	}{
		{"/api/v1/session", []string{http.MethodPost, http.MethodDelete}},
		{"/api/v1/me", []string{http.MethodGet}},
		{"/api/v1/me/password", []string{http.MethodPatch}},
		{"/api/v1/users", []string{http.MethodGet, http.MethodPost}},
		{"/api/v1/users/" + id, []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
		{"/api/v1/groups", []string{http.MethodGet, http.MethodPost}},
		{"/api/v1/groups/" + id, []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
		{"/api/v1/audit", []string{http.MethodGet}},
		{"/api/v1/nodes", []string{http.MethodGet, http.MethodPost}},
		{"/api/v1/nodes/" + id, []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
		{"/api/v1/nodes/" + id + "/config", []string{http.MethodGet, http.MethodPatch}},
		{"/api/v1/nodes/batch", []string{http.MethodPost}},
		{"/api/agent/v1/enroll/check", []string{http.MethodPost}},
		{"/api/v1/enrollments", []string{http.MethodPost}},
	} {
		t.Run(route.path, func(t *testing.T) {
			for _, client := range []struct {
				name   string
				cookie *http.Cookie
				csrf   string
				origin string
			}{
				{"anonymous", nil, "", ""},
				{"invalid session", &http.Cookie{Name: admin.Name, Value: "invalid"}, "", ""},
				{"viewer", viewer, viewerCSRF, publicURL},
				{"admin without Origin or CSRF", admin, "", ""},
				{"admin with valid protection", admin, adminCSRF, publicURL},
			} {
				t.Run(client.name, func(t *testing.T) {
					response := request(t, f.handler, http.MethodPut, route.path, "{}", client.cookie, client.csrf, client.origin)
					apiError(t, response, http.StatusMethodNotAllowed, "method_not_allowed")
					allowed := make(map[string]bool)
					for _, item := range strings.Split(response.Header().Get("Allow"), ",") {
						allowed[strings.TrimSpace(item)] = true
					}
					for _, method := range route.allow {
						if !allowed[method] {
							t.Errorf("Allow %q omits applicable method %s", response.Header().Get("Allow"), method)
						}
					}
					if allowed[http.MethodPut] {
						t.Error("Allow advertises the rejected method")
					}
				})
			}
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		apiError(t, request(t, f.handler, method, "/api/v1/unknown-route", "", nil, "", ""), http.StatusNotFound, "not_found")
	}
}

func TestUIDocumentsDisallowFramingWithCSP(t *testing.T) {
	f := newHTTP(t, 2)
	for _, path := range []string{"/", "/public.html", "/login", "/overview", "/nodes", "/alerts", "/settings", "/200.html", "/login.html"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+path, func(t *testing.T) {
				response := request(t, f.handler, method, path, "", nil, "", "")
				if response.Code != http.StatusOK {
					t.Fatalf("UI document returned %d", response.Code)
				}
				found := false
				for _, policy := range response.Header().Values("Content-Security-Policy") {
					for _, directive := range strings.Split(policy, ";") {
						fields := strings.Fields(directive)
						if len(fields) > 0 && strings.EqualFold(fields[0], "frame-ancestors") {
							found = len(fields) == 2 && fields[1] == "'none'"
							if !found {
								t.Errorf("unsafe frame-ancestors directive: %q", directive)
							}
						}
					}
				}
				if !found {
					t.Error("UI document lacks CSP frame-ancestors 'none'")
				}
			})
		}
	}
}

func TestRequestsLeaveExpiredSessionsToRetention(t *testing.T) {
	f := newHTTP(t, 2)
	cookie, _ := login(t, f, "admin")
	for i := range 2 {
		if _, err := f.db.Exec(`INSERT INTO sessions(token_hash,user_id,expires_at)
			SELECT ?,id,0 FROM users WHERE username = 'admin'`, []byte{byte(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/", "/api/public/v1/nodes", "/api/v1/me"} {
		_ = request(t, f.handler, http.MethodGet, path, "", nil, "", "")
	}
	// Reads, even authenticated ones, must not take the SQLite writer.
	response := request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var before int
	if err := f.db.QueryRow("SELECT count(*) FROM sessions WHERE expires_at <= 0").Scan(&before); err != nil || before != 2 {
		t.Fatalf("request handling cleaned sessions: %d %v", before, err)
	}
	retention, ok := f.handler.(interface {
		CleanupRetention(context.Context, time.Time) (server.RetentionResult, error)
	})
	if !ok {
		t.Fatal("handler does not expose retention cleanup")
	}
	result, err := retention.CleanupRetention(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := f.db.QueryRow("SELECT count(*) FROM sessions WHERE expires_at <= 0").Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 0 || result.Sessions != 2 {
		t.Errorf("retention left %d expired sessions and reported %d removed", expired, result.Sessions)
	}
}

func assertHTTPStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("HTTP status = %d, want %d: %s", response.Code, want, response.Body)
	}
}
