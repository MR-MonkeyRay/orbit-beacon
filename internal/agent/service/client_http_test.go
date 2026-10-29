package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"orbitbeacon/internal/agent/identity"
)

func TestHTTPClientRejectsCAWithoutReadingIt(t *testing.T) {
	state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, supplied := range []bool{false, true} {
		name := "default transport"
		var client *http.Client
		if supplied {
			name = "supplied transport"
			client = &http.Client{}
		}
		t.Run(name, func(t *testing.T) {
			client, err := NewClient(ClientOptions{
				ServerURL: "http://beacon.example.test", CAFile: filepath.Join(t.TempDir(), "missing.pem"),
				HTTPClient: client, Identity: state,
			})
			if err == nil {
				client.Close()
				t.Fatal("HTTP accepted a configured CA file")
			}
			if !strings.Contains(err.Error(), "CA file") || !strings.Contains(err.Error(), "HTTPS") {
				t.Fatalf("expected explicit HTTP/CA configuration conflict before opening the file, got %v", err)
			}
		})
	}
}

func TestClientHTTPTransportAndBearerRedirectPolicy(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "HTTP"
		if secure {
			name = "HTTPS"
		}
		t.Run(name, func(t *testing.T) {
			state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			var redirected atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				redirected.Add(1)
			}))
			defer target.Close()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer transport-test-secret" {
					t.Error("request lost bearer authorization")
				}
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			options := ClientOptions{ServerURL: server.URL, Identity: state}
			if secure {
				options.HTTPClient = server.Client()
			}
			client, err := NewClient(options)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var response struct {
				OK bool `json:"ok"`
			}
			if err := client.post(t.Context(), "/ok", "transport-test-secret", struct{}{}, &response); err != nil || !response.OK {
				t.Fatalf("request failed: response=%+v err=%v", response, err)
			}
			err = client.post(t.Context(), "/redirect", "transport-test-secret", struct{}{}, &response)
			var rejected *HTTPError
			if !errors.As(err, &rejected) || rejected.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 {
				t.Fatalf("bearer redirect was not rejected: err=%v redirected=%d", err, redirected.Load())
			}
		})
	}
}
