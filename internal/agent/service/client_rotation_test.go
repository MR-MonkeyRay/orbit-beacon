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
	state, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := service.OpenConfigStore(filepath.Join(t.TempDir(), "desired.json"), desired(1, 10))
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
	client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Enroll(context.Background(), "initial-ticket"); err != nil {
		t.Fatal(err)
	}
	old := state.Current()
	before := requests.Load()
	// A directory cannot be atomically replaced by an identity file, even as root.
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

func TestMissingReceiptCredentialStateFailsBeforeClaimOrStaging(t *testing.T) {
	state, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := service.OpenConfigStore(filepath.Join(t.TempDir(), "desired.json"), desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	before := state.Current()
	var claims atomic.Int64
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/v1/enroll/check" {
			// An older or incomplete response cannot distinguish a committed
			// credential with a lost local node binding from a new identity.
			_, _ = w.Write([]byte(`{"claimed":false,"server_time":"2026-10-26T12:00:00Z"}`))
			return
		}
		claims.Add(1)
		_, _ = w.Write([]byte(`{"node_id":"already-active-node","server_time":"2026-10-26T12:00:00Z"}`))
	}))
	defer https.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: state, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Enroll(context.Background(), "fresh-ticket"); err == nil {
		t.Fatal("missing credential state was silently treated as inactive")
	}
	if claims.Load() != 0 || state.Current() != before {
		t.Fatal("ambiguous old response allowed credential staging or ticket consumption")
	}
}
