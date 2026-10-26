package server_test

import (
	"context"
	"encoding/json"
	"errors"
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

func TestLateConcurrentRotationCannotRollBackNewCredential(t *testing.T) {
	ctx := context.Background()
	f := newAgentHTTP(t, 1)
	node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "rotation-CAS"})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	late, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	releaseLate := sync.OnceFunc(func() { close(release) })
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/v1/enroll" && r.Header.Get("Authorization") == "Bearer "+late.Token {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(https.Close)
	t.Cleanup(releaseLate)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.json")
	one, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(dir, "desired.json"), v1.DesiredConfig{Revision: 1, ReportIntervalSeconds: 5, DiskIntervalSeconds: 60, BillingDay: 1, QuotaBytes: "0"})
	if err != nil {
		t.Fatal(err)
	}
	newClient := func(state *identity.Store) *service.Client {
		t.Helper()
		client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: state, Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		return client
	}
	clientOne := newClient(one)
	if _, err := clientOne.Enroll(ctx, initial.Token); err != nil {
		t.Fatal(err)
	}
	two, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	clientTwo := newClient(two)
	lateResult := make(chan error, 1)
	go func() { _, err := clientOne.Enroll(ctx, late.Token); lateResult <- err }()
	select {
	case <-entered: // The old candidate is durable and already in flight.
	case err := <-lateResult:
		t.Fatalf("late request failed before reaching the server: %v", err)
	}
	if _, err := clientTwo.Enroll(ctx, first.Token); err != nil {
		t.Fatal(err)
	}
	oldCandidate := two.Current().NodeToken
	if _, err := clientTwo.Enroll(ctx, second.Token); err != nil {
		t.Fatal(err)
	}
	latest := two.Current()
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", latest.NodeToken, reportBody(7, 1, "new-owner", 25)), 1)
	releaseLate()
	lateErr := <-lateResult
	if _, err := f.nodes.AuthenticateToken(ctx, latest.NodeToken); err != nil {
		t.Fatalf("late request rolled back the committed credential: %v", err)
	}
	var rejected *service.HTTPError
	if !errors.As(lateErr, &rejected) || rejected.StatusCode != http.StatusConflict {
		t.Fatalf("stale rotation was not rejected before acknowledgement: %v", lateErr)
	}
	var generation, session string
	if err := f.db.QueryRow("SELECT run_generation,report_session_id FROM nodes WHERE id=?", node.ID).Scan(&generation, &session); err != nil || generation != "7" || session != "new-owner" {
		t.Fatalf("stale rotation reset the ownership fence: generation=%q session=%q err=%v", generation, session, err)
	}
	unused, err := f.enrollments.Get(ctx, late.ID)
	if err != nil || unused.Uses != 0 {
		t.Fatalf("stale rotation consumed its ticket: uses=%d err=%v", unused.Uses, err)
	}
	if _, err := clientOne.Report(ctx, mustReport(t, reportBody(7, 2, "new-owner", 30))); err != nil {
		t.Fatalf("stale process could not reload the winning credential: %v", err)
	}
	apiError(t, agentRequest(t, f, "/api/agent/v1/enroll", first.Token, enrollBody(latest.InstallationID, oldCandidate)), http.StatusConflict, "enrollment_conflict")
}

func TestRotationRequiresCurrentCredentialProofBeforeConsumingTicket(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, active, _ := enrollHTTP(t, f, "proof", 1)
	ticket, err := f.enrollments.CreateSingle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	candidate := agentSecret(2)
	for _, test := range []struct {
		name, previous string
		status         int
	}{
		{"missing", "", http.StatusConflict},
		{"wrong", agentSecret(9), http.StatusConflict},
		{"malformed", "not-a-credential", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := map[string]any{"protocol_version": 1, "installation_id": "installation-proof", "node_token": candidate, "system": v1.System{}}
			if test.previous != "" {
				input["previous_node_token"] = test.previous
			}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			response := agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, string(body))
			assertHTTPStatus(t, response, test.status)
			if _, err := f.nodes.AuthenticateToken(context.Background(), active); err != nil {
				t.Fatal("rejected credential proof revoked the current credential")
			}
			unused, err := f.enrollments.Get(context.Background(), ticket.ID)
			if err != nil || unused.Uses != 0 {
				t.Fatalf("rejected credential proof consumed its ticket: uses=%d err=%v", unused.Uses, err)
			}
		})
	}
}
