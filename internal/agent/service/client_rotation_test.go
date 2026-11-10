package service_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
)

func TestRotationStagingFailureDoesNotSendRequestOrReplaceActiveToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := identity.Open(t.Context(), path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/agent/v1/enroll/check" {
			_, _ = w.Write([]byte(`{"claimed":false,"credential_state":"unbound","server_time":"2026-10-26T12:00:00Z"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_id":"same-node","server_time":"2026-10-26T12:00:00Z"}`))
	}))
	defer server.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Enroll(context.Background(), "initial-ticket"); err != nil {
		t.Fatal(err)
	}
	old := state.Current()
	before := requests.Load()
	// 目录无法被身份文件原子替换，即使以 root 身份也不行。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Enroll(context.Background(), "rotation-ticket"); err == nil {
		t.Fatal("rotation continued despite inability to durably stage candidate")
	}
	if requests.Load() != before {
		t.Fatal("rotation request escaped before candidate was durable")
	}
	if state.Current() != old {
		t.Fatal("failed candidate staging altered active identity")
	}
}

func TestInvalidEnrollmentCheckFailsBeforeClaimOrStaging(t *testing.T) {
	for _, response := range []string{
		`{"claimed":true,"node_id":"already-active-node","credential_state":"active","server_time":"2026-10-26T12:00:00Z"}`,
		`{"claimed":false,"node_id":"already-active-node","credential_state":"active","server_time":"2026-10-26T12:00:00Z"}`,
		`{"claimed":false,"server_time":"2026-10-26T12:00:00Z"}`,
		`{"claimed":false,"credential_state":"unknown","server_time":"2026-10-26T12:00:00Z"}`,
		`{"claimed":false,"credential_state":"unbound"}`,
	} {
		t.Run(response, func(t *testing.T) {
			state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			before := state.Current()
			var claims atomic.Int64
			https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/agent/v1/enroll/check" {
					_, _ = w.Write([]byte(response))
					return
				}
				claims.Add(1)
				_, _ = w.Write([]byte(`{"node_id":"already-active-node","server_time":"2026-10-26T12:00:00Z"}`))
			}))
			defer https.Close()
			client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: state})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Enroll(context.Background(), "fresh-ticket"); err == nil {
				t.Fatal("invalid or legacy claimed check response was accepted")
			}
			if claims.Load() != 0 || state.Current() != before {
				t.Fatal("invalid check response allowed credential staging or ticket consumption")
			}
		})
	}
}

func TestEnrollmentCheckConflictDoesNotRetryWithAnotherCredential(t *testing.T) {
	state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.StageEnrollment(t.Context(), [32]byte{1}, true); err != nil {
		t.Fatal(err)
	}
	before := state.Current()
	var checks, claims atomic.Int64
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/v1/enroll/check" {
			checks.Add(1)
			w.WriteHeader(http.StatusConflict)
			return
		}
		claims.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer https.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: state})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Enroll(t.Context(), "fresh-ticket"); err == nil {
		t.Fatal("conflicting enrollment preflight succeeded")
	}
	if checks.Load() != 1 || claims.Load() != 0 || state.Current() != before {
		t.Fatal("conflicting preflight changed credentials, retried, or consumed its ticket")
	}
}
