package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const routingContractID = "01234567-89ab-4cde-abcd-0123456789ab"

// Most routing decisions need no services. Nil dependencies also prove these
// requests finish before authentication, database access or resource dispatch.
func routingContractHandler(t *testing.T) *handler {
	t.Helper()
	assets, err := prepareStaticAssets(fstest.MapFS{
		"200.html":                   {Data: []byte("console shell")},
		"public.html":                {Data: []byte("public shell")},
		"_app/immutable/app.test.js": {Data: []byte("export const app = true;")},
		"favicon.svg":                {Data: []byte("<svg></svg>")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &handler{origin: "https://beacon.example.test", staticAssets: assets}
}

func routingContractRequest(h *handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://beacon.example.test"+path, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func routingContractResponse(t *testing.T, w *httptest.ResponseRecorder, method string, status int, code, allow string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body)
	}
	if location := w.Header().Get("Location"); location != "" {
		t.Errorf("unexpected redirect Location = %q", location)
	}
	if got := w.Header().Get("Allow"); got != allow {
		t.Errorf("Allow = %q, want %q", got, allow)
	}
	if code != "" {
		if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q, want JSON", got)
		}
		if method != http.MethodHead {
			var body apiError
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != code || body.RequestID == "" || body.RequestID != w.Header().Get("X-Request-ID") {
				t.Errorf("error = %+v, want code %q and correlated request ID", body, code)
			}
		}
	}
	if method == http.MethodHead && w.Body.Len() != 0 {
		t.Errorf("HEAD wrote a body: %q", w.Body.String())
	}
}

// OpenAPI supplies the inventory; this order pins the literal Allow wire value
// independently of the route table.
func routingContractAllow(t *testing.T, methods []string) string {
	t.Helper()
	var ordered []string
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		if slices.Contains(methods, method) {
			ordered = append(ordered, method)
		}
	}
	if len(ordered) != len(methods) {
		t.Fatalf("unrecorded method set: %v", methods)
	}
	return strings.Join(ordered, ", ")
}

func TestRoutingContractMethodsBeforeGates(t *testing.T) {
	h := routingContractHandler(t)
	for path, methods := range openAPIPathOperations(t) {
		t.Run(path, func(t *testing.T) {
			allow := routingContractAllow(t, methods)
			for _, method := range []string{http.MethodPut, http.MethodHead, http.MethodOptions} {
				for _, origin := range []string{"", "https://untrusted.example.test"} {
					t.Run(method+"/origin="+origin, func(t *testing.T) {
						r := httptest.NewRequest(method, concreteOpenAPIPath(path), strings.NewReader(`not json`))
						r.Header.Set("Origin", origin)
						// No session or CSRF token; the body is deliberately invalid.
						w := httptest.NewRecorder()
						h.ServeHTTP(w, r)
						routingContractResponse(t, w, method, 405, "method_not_allowed", allow)
					})
				}
			}
		})
	}
}

// Cancel after the first complete SSE frame, keeping the test synchronous and
// pinning the successful streaming response without waiting for its lifetime.
type routingContractStreamRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w routingContractStreamRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.cancel()
}

func (w routingContractStreamRecorder) SetWriteDeadline(time.Time) error { return nil }

func TestRoutingContractWithDatabase(t *testing.T) {
	h := openAPITestHandler(t)
	for path, methods := range openAPIPathOperations(t) {
		for _, method := range methods {
			t.Run(method+" "+path, func(t *testing.T) {
				status, code := 401, "unauthenticated"
				switch path {
				case "/api/public/v1/nodes", "/api/public/v1/stream":
					status, code = 200, ""
				case "/api/public/v1/ws":
					status, code = 403, "forbidden" // Missing trusted Origin, before upgrade.
				case "/api/v1/session":
					if method == http.MethodPost {
						status, code = 403, "forbidden"
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				r := httptest.NewRequest(method, concreteOpenAPIPath(path), strings.NewReader(`{}`)).WithContext(ctx)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				if path == "/api/public/v1/stream" {
					h.ServeHTTP(routingContractStreamRecorder{w, cancel}, r)
				} else {
					h.ServeHTTP(w, r)
				}
				routingContractResponse(t, w, method, status, code, "")
				if path == "/api/public/v1/stream" || path == "/api/public/v1/nodes" {
					wantType := "application/json; charset=utf-8"
					if path == "/api/public/v1/stream" {
						wantType = "text/event-stream; charset=utf-8"
					}
					if w.Header().Get("Content-Type") != wantType || !strings.Contains(w.Body.String(), `"type":"snapshot"`) {
						t.Fatalf("expected public snapshot, got %v %s", w.Header(), w.Body)
					}
				}
			})
		}
	}
	t.Run("login with trusted Origin and empty credentials", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{}`))
		r.Header.Set("Origin", h.origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		routingContractResponse(t, w, http.MethodPost, 401, "unauthenticated", "")
	})
	// Health remains public when the public-page gate is closed.
	h.publicPageDisabled.Store(true)
	for _, ready := range []bool{true, false} {
		if !ready {
			h.MarkNotReady()
		}
		for _, path := range []string{"/healthz/live", "/healthz/ready"} {
			status, state := 200, "ok"
			if path == "/healthz/ready" {
				state = "ready"
				if !ready {
					status, state = 503, "not_ready"
				}
			}
			w := routingContractRequest(h, http.MethodGet, path)
			routingContractResponse(t, w, http.MethodGet, status, "", "")
			if want := fmt.Sprintf("{\"status\":%q}\n", state); w.Body.String() != want {
				t.Errorf("%s ready=%v: body = %q, want %q", path, ready, w.Body.String(), want)
			}
		}
	}
}

func TestRoutingContractUnknownAPIPaths(t *testing.T) {
	h := routingContractHandler(t)
	paths := []string{
		"/api", "/api/", "/api/unknown", "/api/v1/unknown", "/api/agent/v1/unknown", "/api/public/v1/unknown",
		"/api//v1/nodes", "/api/v1//nodes", "/api/v1/nodes//" + routingContractID,
		"/api/v1/nodes/" + routingContractID + "/extra", "/api/v1/nodes/01234567%2F89ab-4cde-abcd-0123456789ab",
		"/api/v1/nodes/%2F", "/api/v1/nodes/%252F", "/api/v1/nodes/../nodes",
	}
	for path := range openAPIPathOperations(t) {
		paths = append(paths, concreteOpenAPIPath(path)+"/")
		if strings.Contains(path, "{id}") {
			for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000", "0123456789ab4cdeabcd0123456789ab", "%7B" + routingContractID + "%7D"} {
				paths = append(paths, strings.ReplaceAll(path, "{id}", id))
			}
		}
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodHead, http.MethodOptions} {
				w := routingContractRequest(h, method, path)
				routingContractResponse(t, w, method, 404, "not_found", "")
			}
		})
	}
}

func TestRoutingContractAcceptedPathSpellings(t *testing.T) {
	h := routingContractHandler(t)
	for _, path := range []string{
		"/api/v1/nodes/" + routingContractID,
		"/api/v1/nodes/" + strings.ToUpper(routingContractID),
		"/api/v1/nodes/01234567-89%61b-4cde-abcd-0123456789ab",
		"/api/%761/nodes",
	} {
		t.Run(path, func(t *testing.T) {
			routingContractResponse(t, routingContractRequest(h, http.MethodGet, path), http.MethodGet, 401, "unauthenticated", "")
		})
	}
}

func TestRoutingContractEncodedSeparatorsAreNotRouteBoundaries(t *testing.T) {
	h := routingContractHandler(t)
	// ServeMux matches escaped segments: encoded separators cannot create routes.
	for _, tc := range []struct {
		path   string
		status int
		code   string
	}{
		{"/api%2Fv1%2Fnodes", 404, "not_found"},
		{"/api/v1/nodes%2F" + routingContractID, 404, "not_found"},
		{"/api/v1/nodes/" + routingContractID + "%2Fruntime", 404, "not_found"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			routingContractResponse(t, routingContractRequest(h, http.MethodGet, tc.path), http.MethodGet, tc.status, tc.code, "")
		})
	}
}

func TestRoutingContractStaticAndHealthMethods(t *testing.T) {
	h := routingContractHandler(t)
	for _, tc := range []struct {
		path, body string
		status     int
		json       bool
	}{
		{"/healthz/live", "{\"status\":\"ok\"}\n", 200, true},
		{"/healthz/ready", "{\"status\":\"not_ready\"}\n", 503, true},
		{"/", "public shell", 200, false},
		{"/login", "console shell", 200, false},
		{"/nodes/" + routingContractID, "console shell", 200, false},
		{"/_app/immutable/app.test.js", "export const app = true;", 200, false},
		{"/favicon.svg", "<svg></svg>", 200, false},
		{"/missing", "404 page not found\n", 404, false},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
			t.Run(method+" "+tc.path, func(t *testing.T) {
				status, code, allow, body := tc.status, "", "", tc.body
				if tc.json && method != http.MethodGet {
					status, code, allow = 405, "method_not_allowed", "GET"
				} else if !tc.json && method == http.MethodOptions {
					status, allow, body = 405, "GET, HEAD", "Method not allowed\n"
				}
				w := routingContractRequest(h, method, tc.path)
				routingContractResponse(t, w, method, status, code, allow)
				if code == "" && method != http.MethodHead && w.Body.String() != body {
					t.Errorf("body = %q, want %q", w.Body.String(), body)
				}
				if method == http.MethodHead && !tc.json && tc.status == 200 {
					if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(tc.body)) {
						t.Errorf("HEAD Content-Length = %q, want %d", got, len(tc.body))
					}
				}
			})
		}
	}
}

func TestRoutingContractConsolePaths(t *testing.T) {
	h := routingContractHandler(t)
	console := []string{"/login", "/change-password", "/overview", "/nodes", "/alerts", "/settings", "/nodes/" + routingContractID, "/nodes/" + strings.ToUpper(routingContractID)}
	missing := []string{"/users", "/groups", "/tasks", "/notifications", "/backups", "/console", "/overview/extra", "/nodes/batch", "/nodes/not-a-uuid", "/nodes/00000000-0000-0000-0000-000000000000", "/nodes/" + routingContractID + "/runtime", "/nodes/" + routingContractID + "%2Fruntime", "/nodes//" + routingContractID, "//nodes", "/nodes/../settings"}
	for _, path := range console {
		missing = append(missing, path+"/")
	}
	for _, group := range []struct {
		paths  []string
		status int
		body   string
	}{
		{console, 200, "console shell"},
		{[]string{"/200.html"}, 200, "console shell"},
		{[]string{"/", "/public.html"}, 200, "public shell"},
		{missing, 404, "404 page not found\n"},
	} {
		for _, path := range group.paths {
			t.Run(path, func(t *testing.T) {
				w := routingContractRequest(h, http.MethodGet, path)
				routingContractResponse(t, w, http.MethodGet, group.status, "", "")
				if w.Body.String() != group.body {
					t.Errorf("body = %q, want %q", w.Body.String(), group.body)
				}
			})
		}
	}
}

func TestRoutingContractPublicPageGate(t *testing.T) {
	h := routingContractHandler(t)
	h.publicPageDisabled.Store(true)
	for _, path := range []string{"/api/public/v1/nodes", "/api/public/v1/stream", "/api/public/v1/ws"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodHead, http.MethodOptions} {
			t.Run(method+" "+path, func(t *testing.T) {
				status, code, allow := 405, "method_not_allowed", "GET"
				if method == http.MethodGet {
					status, code, allow = 403, "public_page_disabled", ""
				}
				// Invalid pagination and absent WebSocket Origin cannot precede the gate.
				w := routingContractRequest(h, method, path+"?limit=invalid")
				routingContractResponse(t, w, method, status, code, allow)
			})
		}
	}
}

func TestRoutingContractLabels(t *testing.T) {
	// Every documented concrete route should retain its OpenAPI template label,
	// including summary, batch, preview and read-all literal exceptions.
	labels := make(map[string]string)
	for path := range openAPIPathOperations(t) {
		labels[concreteOpenAPIPath(path)] = path
	}
	for path, label := range map[string]string{
		"/api/v1/nodes/" + strings.ToUpper(routingContractID) + "/runtime": "/api/v1/nodes/{id}/runtime",
		"/api/v1/nodes/not-a-uuid":    "unmatched",
		"/api/v1/nodes/":              "unmatched",
		"/api":                        "unmatched",
		"/unknown":                    "unmatched",
		"/healthz/live":               "/healthz/live",
		"/healthz/ready":              "/healthz/ready",
		"/":                           "/",
		"/login":                      "/login",
		"/change-password":            "/change-password",
		"/overview":                   "/overview",
		"/nodes":                      "/nodes",
		"/alerts":                     "/alerts",
		"/settings":                   "/settings",
		"/nodes/" + routingContractID: "/nodes/{id}",
		"/nodes/not-a-uuid":           "/nodes/{id}", // Labeling is broader than dispatch.
		"/nodes/":                     "/nodes/{id}",
		"/200.html":                   "unmatched",
		"/public.html":                "unmatched",
		"/favicon.svg":                "unmatched",
		"/_app/immutable/app.test.js": "/_app/immutable/{asset}",
		"/_app/immutable/missing.js":  "/_app/immutable/{asset}",
	} {
		labels[path] = label
	}
	for path, label := range labels {
		t.Run(path, func(t *testing.T) {
			if got := requestRoute(path); got != label {
				t.Errorf("requestRoute = %q, want %q", got, label)
			}
			var logs bytes.Buffer
			h := routingContractHandler(t)
			h.accessLog, h.metrics = true, newHTTPMetrics("test")
			h.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			// PUT keeps API paths before services. Health GET also pins filtering
			// of successful probes and logging of failed readiness probes.
			method := http.MethodPut
			if healthProbe(path) {
				method = http.MethodGet
			}
			w := routingContractRequest(h, method, path+"?private=query")
			if healthProbe(path) && w.Code < 500 {
				if logs.Len() != 0 {
					t.Errorf("health method rejection logged: %s", &logs)
				}
			} else {
				var record struct {
					Route string `json:"route"`
				}
				if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record.Route != label {
					t.Errorf("logged route = %q, want %q", record.Route, label)
				}
			}
			want := fmt.Sprintf("orbitbeacon_http_requests_total{method=%q,route=%q,status_class=\"%dxx\"} 1\n", method, label, w.Code/100)
			if got := h.metrics.exposition(); !strings.Contains(got, want) {
				t.Errorf("metrics missing %q:\n%s", want, got)
			}
		})
	}
}
