package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

type enrollmentResponseDropper struct {
	base http.RoundTripper
	drop bool
}

func (d *enrollmentResponseDropper) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := d.base.RoundTrip(r)
	if err == nil && d.drop && r.URL.Path == "/api/agent/v1/enroll" {
		d.drop = false
		_ = response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, err
}

func clientState(t *testing.T) (*identity.Store, *service.ConfigStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := service.OpenConfigStore(filepath.Join(t.TempDir(), "desired.json"), v1.DesiredConfig{Revision: 1, ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, DiskPaths: []string{}, Interfaces: []string{}, ProbeTargets: []string{}, BillingDay: 1, QuotaBytes: "0"})
	if err != nil {
		t.Fatal(err)
	}
	return state, cfg, path
}

func TestClientRotationStagesFreshCredentialBeforeRequestAndPromotesAfterExactRetry(t *testing.T) {
	f := newAgentHTTP(t, 1)
	ctx := context.Background()
	node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "rotating client"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, cfg, path := clientState(t)
	var mu sync.Mutex
	var staged []string
	var oldToken string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/v1/enroll" && oldToken != "" {
			var request v1.EnrollmentRequest
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			decoded, err := base64.RawURLEncoding.DecodeString(request.NodeToken)
			if err != nil || len(decoded) != 32 || request.NodeToken == oldToken {
				t.Error("rotation did not generate a fresh 256-bit token")
			}
			persisted, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			var disk map[string]any
			if err := json.Unmarshal(persisted, &disk); err != nil {
				t.Error(err)
			}
			if disk["node_token"] != oldToken || disk["pending_node_token"] != request.NodeToken {
				t.Error("candidate not durably staged separately from active token before request")
			}
			mu.Lock()
			staged = append(staged, request.NodeToken)
			mu.Unlock()
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	transport := &enrollmentResponseDropper{base: server.Client().Transport}
	newClient := func(store *identity.Store) *service.Client {
		client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: &http.Client{Transport: transport}, Identity: store, Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	client := newClient(state)
	defer client.Close()
	if _, err := client.Enroll(ctx, first.Token); err != nil {
		t.Fatal(err)
	}
	oldToken = state.Current().NodeToken
	rotation, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	transport.drop = true
	if _, err := client.Enroll(ctx, rotation.Token); err == nil {
		t.Fatal("lost response incorrectly reported success")
	}
	if state.Current().NodeToken != oldToken {
		t.Fatal("unacknowledged rotation replaced active token")
	}
	reopened, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	retry := newClient(reopened)
	defer retry.Close()
	if _, err := retry.Enroll(ctx, rotation.Token); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(staged) != 2 || staged[0] != staged[1] {
		t.Fatalf("rotation retry changed candidate: %v", staged)
	}
	candidate := staged[0]
	mu.Unlock()
	if reopened.Current().NodeToken != candidate || candidate == oldToken {
		t.Fatal("acknowledged fresh credential not promoted")
	}
	durable, err := identity.Open(path, identity.Options{})
	if err != nil || durable.Current().NodeToken != candidate {
		t.Fatalf("promotion not durable: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var promoted map[string]any
	if err := json.Unmarshal(data, &promoted); err != nil {
		t.Fatal(err)
	}
	if pending, exists := promoted["pending_node_token"]; exists && pending != "" && pending != nil {
		t.Fatal("successful rotation retained staged credential")
	}
	if _, err := f.nodes.AuthenticateToken(ctx, oldToken); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Fatalf("old credential still authenticates: %v", err)
	}
	if got, err := f.nodes.AuthenticateToken(ctx, candidate); err != nil || got.ID != node.ID {
		t.Fatalf("new credential not bound: %+v %v", got, err)
	}
}

func TestClientFailedOrRevokedRotationKeepsOldActiveCredential(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "server failure", true: "revoked ticket"}[revoked], func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			ctx := context.Background()
			state, cfg, path := clientState(t)
			node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "failed rotation"})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			var fail bool
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail && r.URL.Path == "/api/agent/v1/enroll" {
					if !revoked {
						w.WriteHeader(503)
						return
					}
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Enroll(ctx, initial.Token); err != nil {
				t.Fatal(err)
			}
			old := state.Current().NodeToken
			rotation, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if revoked {
				if err := f.enrollments.Revoke(ctx, rotation.ID); err != nil {
					t.Fatal(err)
				}
			}
			fail = true
			if _, err := client.Enroll(ctx, rotation.Token); err == nil {
				t.Fatal("failed rotation succeeded")
			}
			reopened, err := identity.Open(path, identity.Options{})
			if err != nil || state.Current().NodeToken != old || reopened.Current().NodeToken != old {
				t.Fatalf("failed rotation destroyed active credential: %v", err)
			}
			if got, err := f.nodes.AuthenticateToken(ctx, old); err != nil || got.ID != node.ID {
				t.Fatalf("failed rotation revoked active server token: %v", err)
			}
		})
	}
}
