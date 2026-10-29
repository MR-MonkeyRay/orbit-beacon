package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"orbitbeacon/internal/server"
)

const publicPageRoute = "/api/v1/settings/public-page"

func TestPublicPageRootMatrixAndConditionalRequests(t *testing.T) {
	f := newHTTP(t, 2)
	admin, csrf := login(t, f, "admin")
	if _, err := f.auth.CreateUser(context.Background(), "restricted", password, true); err != nil {
		t.Fatal(err)
	}
	restricted, _ := login(t, f, "restricted")
	for _, enabled := range []bool{true, false} {
		assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, fmt.Sprintf(`{"enabled":%t}`, enabled), admin, csrf, publicURL), http.StatusOK)
		for _, state := range []struct {
			name   string
			cookie *http.Cookie
			target string
		}{
			{"anonymous", nil, "/login"}, {"normal", admin, "/overview"}, {"restricted", restricted, "/change-password"},
			{"invalid", &http.Cookie{Name: admin.Name, Value: "invalid"}, "/login"},
		} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				for _, path := range []string{"/", "/public.html"} {
					t.Run(fmt.Sprintf("%t/%s/%s/%s", enabled, state.name, method, path), func(t *testing.T) {
						r := httptest.NewRequest(method, publicURL+path, nil)
						if state.cookie != nil {
							r.AddCookie(state.cookie)
						}
						w := httptest.NewRecorder()
						f.handler.ServeHTTP(w, r)
						if w.Header().Get("Cache-Control") != "no-store" {
							t.Fatal("root response permits caching")
						}
						if enabled {
							assertHTTPStatus(t, w, http.StatusOK)
							if w.Header().Get("Location") != "" {
								t.Fatal("enabled public page redirected authenticated visitor")
							}
						} else {
							assertHTTPStatus(t, w, http.StatusFound)
							if w.Header().Get("Location") != state.target {
								t.Fatalf("redirect=%q want=%q", w.Header().Get("Location"), state.target)
							}
						}
						if method == http.MethodHead && w.Body.Len() != 0 {
							t.Fatal("HEAD returned a body")
						}
					})
				}
			}
		}
	}
	// Old public ETags must never bypass the disabled gate.
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":true}`, admin, csrf, publicURL), 200)
	etag := request(t, f.handler, http.MethodGet, "/", "", nil, "", "").Header().Get("ETag")
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":false}`, admin, csrf, publicURL), 200)
	for _, path := range []string{"/", "/public.html"} {
		r := httptest.NewRequest(http.MethodGet, publicURL+path, nil)
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		assertHTTPStatus(t, w, 302)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodPost, "/", "", nil, "", ""), 405)
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/unknown-public-path", "", nil, "", ""), 404)
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/_app/immutable/main.js", "", nil, "", ""), 200)
}

func TestPublicPageHTTPValidationAuthorizationAndRollback(t *testing.T) {
	f := newHTTP(t, 2)
	admin, csrf := login(t, f, "admin")
	apiError(t, request(t, f.handler, http.MethodGet, publicPageRoute, "", nil, "", ""), 401, "unauthenticated")
	apiError(t, request(t, f.handler, http.MethodPost, publicPageRoute, "{}", nil, "", ""), 405, "method_not_allowed")
	apiError(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":false}`, admin, "", publicURL), 403, "forbidden")
	apiError(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":false}`, admin, csrf, "https://evil.test"), 403, "forbidden")
	if _, err := f.auth.CreateUser(context.Background(), "restricted", password, true); err != nil {
		t.Fatal(err)
	}
	restricted, restrictedCSRF := login(t, f, "restricted")
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		apiError(t, request(t, f.handler, method, publicPageRoute, `{"enabled":false}`, restricted, restrictedCSRF, publicURL), 403, "password_change_required")
	}
	for _, body := range []string{`{}`, `null`, `{"enabled":null}`, `{"enabled":"false"}`, `{"enabled":0}`, `{"enabled":false,"unknown":true}`, `[]`} {
		assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, body, admin, csrf, publicURL), 400)
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM settings WHERE key='public_page_enabled'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid writes persisted: %d %v", count, err)
	}
	before := f.nodes.PublicSafetyVersion()
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":true}`, admin, csrf, publicURL), 200)
	if f.nodes.PublicSafetyVersion() != before {
		t.Fatal("idempotent PATCH invalidated public streams")
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_public_page_audit BEFORE INSERT ON audit_events WHEN NEW.action='public_page.update' BEGIN SELECT RAISE(ABORT,'audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":false}`, admin, csrf, publicURL), 500)
	if f.nodes.PublicSafetyVersion() != before {
		t.Fatal("failed transaction invalidated public streams")
	}
	if got := request(t, f.handler, http.MethodGet, publicPageRoute, "", admin, "", ""); !strings.Contains(got.Body.String(), `"enabled":true`) {
		t.Fatalf("failed mutation changed runtime: %s", got.Body)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/", "", nil, "", ""), 200)
	if _, err := f.db.Exec("DROP TRIGGER reject_public_page_audit"); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":false}`, admin, csrf, publicURL), 200)
	if f.nodes.PublicSafetyVersion() == before {
		t.Fatal("successful transition did not invalidate public streams")
	}
	before = f.nodes.PublicSafetyVersion()
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":false}`, admin, csrf, publicURL), 200)
	if f.nodes.PublicSafetyVersion() != before {
		t.Fatal("repeated disabled PATCH invalidated public streams")
	}
	for _, path := range []string{"/api/public/v1/nodes", "/api/public/v1/stream", "/api/public/v1/ws"} {
		for _, cookie := range []*http.Cookie{nil, admin, restricted} {
			apiError(t, request(t, f.handler, http.MethodGet, path, "", cookie, "", ""), 403, "public_page_disabled")
		}
	}
	if _, err := f.db.Exec("UPDATE settings SET value='true' WHERE key='public_page_enabled'"); err != nil {
		t.Fatal(err)
	}
	// GET uses published runtime, not a second database read.
	if got := request(t, f.handler, http.MethodGet, publicPageRoute, "", admin, "", ""); !strings.Contains(got.Body.String(), `"enabled":false`) {
		t.Fatalf("GET bypassed runtime: %s", got.Body)
	}
}

func TestPublicPageStartupPersistenceCorruptionAndRootDatabaseFailure(t *testing.T) {
	f := newHTTP(t, 2)
	admin, csrf := login(t, f, "admin")
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, publicPageRoute, `{"enabled":false}`, admin, csrf, publicURL), 200)
	h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, h, http.MethodGet, "/", "", nil, "", ""), 302)
	if _, err := f.db.Exec("UPDATE settings SET value='TRUE' WHERE key='public_page_enabled'"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL}); err == nil {
		t.Fatal("corrupt public setting allowed startup")
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, h, http.MethodGet, "/", "", admin, "", ""), 500)
}

func TestPublicPageConcurrentMutationsPublishLastCommittedState(t *testing.T) {
	f := newHTTP(t, 2)
	admin, csrf := login(t, f, "admin")
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(enabled bool) {
			defer wg.Done()
			w := request(t, f.handler, http.MethodPatch, publicPageRoute, fmt.Sprintf(`{"enabled":%t}`, enabled), admin, csrf, publicURL)
			if w.Code != 200 {
				t.Errorf("mutation=%d %s", w.Code, w.Body)
			}
		}(i%2 == 0)
	}
	wg.Wait()
	var persisted string
	if err := f.db.QueryRow("SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	got := request(t, f.handler, http.MethodGet, publicPageRoute, "", admin, "", "")
	if !strings.Contains(got.Body.String(), `"enabled":`+persisted) {
		t.Fatalf("runtime=%s persisted=%s", got.Body, persisted)
	}
	root := request(t, f.handler, http.MethodGet, "/", "", nil, "", "")
	if (root.Code == 200) != (persisted == "true") {
		t.Fatalf("root=%d persisted=%s", root.Code, persisted)
	}
}
