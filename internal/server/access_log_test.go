package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestAccessLogRedactsPrivateDataAndCorrelatesResponse(t *testing.T) {
	var logs bytes.Buffer
	h := &handler{accessLog: true, metrics: newHTTPMetrics("test"), logger: slog.New(slog.NewJSONHandler(&logs, nil)), trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	for _, path := range []string{
		"/api/v1/nodes/00000000-0000-4000-8000-000000000001/runtime?token=private-query",
		"/api/agent/v1/enroll/private-enrollment-token?other=private-query",
		"/_app/immutable/private-filename?secret=private-query",
	} {
		logs.Reset()
		request := httptest.NewRequest(http.MethodGet, "https://beacon.test"+path, nil)
		request.RemoteAddr = "10.1.2.3:1234"
		request.Header.Set("X-Forwarded-For", "198.51.100.42, 10.2.3.4")
		request.Header.Set("Authorization", "Bearer private-auth")
		request.Header.Set("Cookie", "session=private-cookie")
		request.Header.Set("X-Request-ID", "private-request-id")
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if strings.Count(logs.String(), "\n") != 1 {
			t.Fatalf("expected one log line: %s", logs.String())
		}
		var fields map[string]any
		if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"method", "route", "status", "bytes", "duration_ms", "request_id", "client_addr"} {
			if _, ok := fields[name]; !ok {
				t.Errorf("missing field %s: %s", name, logs.String())
			}
		}
		if fields["method"] != http.MethodGet || fields["status"] != float64(response.Code) || fields["bytes"] != float64(response.Body.Len()) || fields["client_addr"] != "198.51.100.42:0" || fields["duration_ms"].(float64) < 0 {
			t.Fatalf("incorrect access fields: %v", fields)
		}
		if fields["request_id"] == "" || fields["request_id"] != response.Header().Get("X-Request-ID") {
			t.Fatalf("uncorrelated request ID: %v", fields)
		}
		for _, secret := range []string{"private-query", "private-enrollment-token", "private-filename", "private-auth", "private-cookie", "private-request-id", "00000000-0000-4000-8000-000000000001", "?"} {
			if strings.Contains(logs.String(), secret) {
				t.Errorf("log contains private input %q: %s", secret, logs.String())
			}
		}
	}
}

func TestAccessLogDisabledAndHealthProbeFiltering(t *testing.T) {
	var logs bytes.Buffer
	h := &handler{metrics: newHTTPMetrics("test"), logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))
	if logs.Len() != 0 {
		t.Fatalf("access logs enabled by default: %s", logs.String())
	}
	h.accessLog = true
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz/live", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/healthz/live", nil))
	if logs.Len() != 0 {
		t.Fatalf("healthy probe logged: %s", logs.String())
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz/ready", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(logs.String(), `"status":503`) {
		t.Fatalf("unhealthy probe not logged: %s", logs.String())
	}
}

func TestRequestRouteUsesOnlyKnownTemplates(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{"/api/v1/nodes/00000000-0000-4000-8000-000000000001/runtime", "/api/v1/nodes/{id}/runtime"},
		{"/api/v1/nodes/batch", "/api/v1/nodes/batch"},
		{"/api/v1/users/00000000-0000-4000-8000-000000000001", "/api/v1/users/{id}"},
		{"/api/v1/nodes/secret/runtime", "unmatched"},
		{"/api/v1/enrollments/secret", "unmatched"},
		{"/unknown/secret", "unmatched"},
	} {
		if got := requestRoute(test.path); got != test.want {
			t.Errorf("requestRoute(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestResponseLogTracksFinalStatusAndWrittenBytes(t *testing.T) {
	recorder := httptest.NewRecorder()
	response := &responseLog{ResponseWriter: recorder}
	response.WriteHeader(http.StatusCreated)
	response.WriteHeader(http.StatusInternalServerError)
	n, err := response.Write([]byte("created"))
	if err != nil || n != 7 || response.status != http.StatusCreated || response.bytes != 7 || recorder.Code != http.StatusCreated {
		t.Fatalf("incorrect response accounting: %+v %d %v", response, n, err)
	}
	if response.Unwrap() != recorder {
		t.Fatal("response controller cannot unwrap recorder")
	}
}

func TestAccessLogAccountsForPanicWithoutSwallowingIt(t *testing.T) {
	var logs bytes.Buffer
	h := &handler{accessLog: true, metrics: newHTTPMetrics("test"), logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	func() {
		defer func() {
			if recover() != "private-panic-text" {
				t.Error("HTTP panic was swallowed or changed")
			}
		}()
		h.observeRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz/live", nil), func(http.ResponseWriter, *http.Request) { panic("private-panic-text") })
	}()
	if !strings.Contains(logs.String(), `"status":500`) || strings.Contains(logs.String(), "private-panic-text") {
		t.Fatalf("incorrect panic accounting: %s", logs.String())
	}
	if h.metrics.inflight.Load() != 0 {
		t.Error("panic leaked in-flight request count")
	}
}
