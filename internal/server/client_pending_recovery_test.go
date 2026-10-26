package server_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	"orbitbeacon/internal/server/nodes"
)

func TestLostRotationResponseRecoversPendingCredentialWithoutTicketOrNewGeneration(t *testing.T) {
	for _, started := range []bool{false, true} {
		name := "enroll-only identity"
		if started {
			name = "previously started identity"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newAgentHTTP(t, 1)
			state, cfg, path := clientState(t)
			if started {
				if _, err := state.BeginRun(); err != nil {
					t.Fatal(err)
				}
			}
			node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: name})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			var recovering atomic.Bool
			var recoveryRequests atomic.Int64
			var candidate string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if recovering.Load() {
					recoveryRequests.Add(1)
					if r.Method != http.MethodPost {
						t.Errorf("node recovery used %s instead of a bounded POST", r.Method)
					}
					if r.URL.Path == "/api/agent/v1/enroll" {
						t.Error("ticketless recovery replayed a removed enrollment ticket")
					}
					if r.Header.Get("Authorization") != "Bearer "+candidate {
						t.Error("recovery did not authenticate the committed pending token")
					}
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			dropper := &enrollmentResponseDropper{base: server.Client().Transport}
			client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: &http.Client{Transport: dropper}, Identity: state, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Enroll(ctx, initial.Token); err != nil {
				t.Fatal(err)
			}
			before := state.Current()
			rotation, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			dropper.drop = true
			if _, err := client.Enroll(ctx, rotation.Token); err == nil {
				t.Fatal("lost rotation response reported success")
			}
			candidate = state.Current().PendingNodeToken
			if candidate == "" || candidate == before.NodeToken || state.Current().NodeToken != before.NodeToken {
				t.Fatal("lost response did not preserve distinct durable active and pending credentials")
			}
			if got, err := f.nodes.AuthenticateToken(ctx, candidate); err != nil || got.ID != node.ID {
				t.Fatalf("rotation did not commit before losing response: %v", err)
			}
			if _, err := f.nodes.AuthenticateToken(ctx, before.NodeToken); !errors.Is(err, nodes.ErrUnauthenticated) {
				t.Fatalf("old token unexpectedly still valid: %v", err)
			}
			if _, err := f.db.Exec("DELETE FROM enrollment_tokens WHERE id IN (?, ?)", initial.ID, rotation.ID); err != nil {
				t.Fatal(err)
			}
			reopened, err := identity.Open(path, identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			retry, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: reopened, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer retry.Close()
			var generationBefore, sessionBefore string
			var seenBefore sql.NullString
			if err := f.db.QueryRow("SELECT run_generation, report_session_id, last_seen FROM nodes WHERE id=?", node.ID).Scan(&generationBefore, &sessionBefore, &seenBefore); err != nil {
				t.Fatal(err)
			}
			recovering.Store(true)
			response, err := retry.Enroll(ctx, "")
			if err != nil || response.NodeID != node.ID {
				t.Fatalf("ticketless invocation could not recover committed pending rotation: response=%+v err=%v", response, err)
			}
			if recoveryRequests.Load() == 0 {
				t.Fatal("recovery promoted a credential without authenticating it at the server")
			}
			durable, err := identity.Open(path, identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			after := durable.Current()
			if after.NodeToken != candidate || after.PendingNodeToken != "" || after.NodeID != node.ID {
				t.Fatal("ticketless recovery did not durably promote exactly the committed pending token")
			}
			if after.RunGeneration != before.RunGeneration || after.SessionID != before.SessionID || after.InstallationID != before.InstallationID {
				t.Fatal("recovery manufactured a new runtime generation, session, or installation")
			}
			var generationAfter, sessionAfter string
			var seenAfter sql.NullString
			if err := f.db.QueryRow("SELECT run_generation, report_session_id, last_seen FROM nodes WHERE id=?", node.ID).Scan(&generationAfter, &sessionAfter, &seenAfter); err != nil {
				t.Fatal(err)
			}
			if generationAfter != generationBefore || sessionAfter != sessionBefore || seenAfter != seenBefore {
				t.Fatal("credential recovery mutated server report ownership or liveness instead of only confirming the local pending token")
			}
			var count int
			if err := f.db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil || count != 1 {
				t.Fatalf("recovery duplicated node: count=%d err=%v", count, err)
			}
		})
	}
}

func TestTicketlessEnrollmentWithoutRecoverableCredentialRequiresTicket(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh identity", true: "rejected active and pending credentials"}[started], func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			state, cfg, _ := clientState(t)
			if started {
				if _, err := state.BeginRun(); err != nil {
					t.Fatal(err)
				}
				if _, _, err := state.StageEnrollment([32]byte{1}, true); err != nil {
					t.Fatal(err)
				}
			}
			before := state.Current()
			server := httptest.NewTLSServer(f.handler)
			defer server.Close()
			client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.Enroll(context.Background(), "")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "ticket") || !strings.Contains(strings.ToLower(err.Error()), "required") {
				t.Fatalf("missing recovery credential needs an actionable ticket-required error: %v", err)
			}
			if state.Current() != before {
				t.Fatal("failed ticketless recovery changed durable credentials or generation")
			}
		})
	}
}
