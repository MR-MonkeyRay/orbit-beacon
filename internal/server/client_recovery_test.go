package server_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	"orbitbeacon/internal/server/nodes"
)

func TestLostEnrollmentResponseRecoversByNodeCredentialWithoutTicket(t *testing.T) {
	for _, disposition := range []string{"revoked", "removed"} {
		t.Run(disposition, func(t *testing.T) {
			ctx := context.Background()
			f := newAgentHTTP(t, 1)
			state, path := clientState(t)
			run, err := state.BeginRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "lost response"})
			if err != nil {
				t.Fatal(err)
			}
			ticket, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var recovery bool
			var recoveryPaths []string
			var recoveryCalls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/agent/v1/recover" {
					recoveryCalls.Add(1)
				}
				mu.Lock()
				if recovery {
					recoveryPaths = append(recoveryPaths, r.URL.Path)
					if r.URL.Path == "/api/agent/v1/enroll" {
						t.Error("recovery required/replayed revoked or removed enrollment ticket")
					}
					if r.Header.Get("Authorization") != "Bearer "+run.NodeToken {
						t.Error("recovery did not authenticate durable node token")
					}
				}
				mu.Unlock()
				f.handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			transport := &enrollmentResponseDropper{base: server.Client().Transport, drop: true}
			client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: &http.Client{Transport: transport}, Identity: state})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Enroll(ctx, ticket.Token); err == nil {
				t.Fatal("expected enrollment response to be lost")
			}
			if got, err := f.nodes.AuthenticateToken(ctx, run.NodeToken); err != nil || got.ID != node.ID {
				t.Fatalf("claim did not commit before response loss: %v", err)
			}
			staged := state.Current()
			_, err = client.Enroll(ctx, ticket.Token)
			var rejected *service.HTTPError
			if !errors.As(err, &rejected) || rejected.StatusCode != http.StatusForbidden {
				t.Fatalf("spent explicit ticket did not return 403: %v", err)
			}
			if recoveryCalls.Load() != 0 || state.Current() != staged {
				t.Fatal("explicit rejection invoked recovery or changed the durable candidate")
			}
			if disposition == "revoked" {
				if err := f.enrollments.Revoke(ctx, ticket.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.db.Exec("DELETE FROM enrollment_tokens WHERE id=?", ticket.ID); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := identity.Open(t.Context(), path, identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			retry, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: reopened})
			if err != nil {
				t.Fatal(err)
			}
			defer retry.Close()
			var epochBefore, epochAfter int64
			var generationBefore, generationAfter, sessionBefore, sessionAfter string
			var seenBefore, seenAfter sql.NullString
			if err := f.db.QueryRow("SELECT credential_epoch, run_generation, report_session_id, last_seen FROM nodes WHERE id=?", node.ID).Scan(&epochBefore, &generationBefore, &sessionBefore, &seenBefore); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			recovery = true
			mu.Unlock()
			_, err = retry.Enroll(ctx, "") // No ticket remains available to this invocation.
			if err != nil {
				t.Fatalf("committed claim recovery required an enrollment ticket: %v", err)
			}
			mu.Lock()
			if len(recoveryPaths) == 0 {
				t.Error("recovery trusted local state without authenticating node at server")
			}
			mu.Unlock()
			if reopened.Current().RunGeneration != run.RunGeneration || reopened.Current().SessionID != run.SessionID {
				t.Fatal("recovery began a second runtime generation")
			}
			if err := f.db.QueryRow("SELECT credential_epoch, run_generation, report_session_id, last_seen FROM nodes WHERE id=?", node.ID).Scan(&epochAfter, &generationAfter, &sessionAfter, &seenAfter); err != nil {
				t.Fatal(err)
			}
			if epochAfter != epochBefore || generationAfter != generationBefore || sessionAfter != sessionBefore || seenAfter != seenBefore {
				t.Fatal("independent recovery changed server ownership or liveness")
			}
			var count int
			if err := f.db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil || count != 1 {
				t.Fatalf("recovery duplicated node: count=%d err=%v", count, err)
			}
			if _, err := retry.Report(ctx, mustReport(t, reportBody(run.RunGeneration, 1, run.SessionID, 10))); err != nil {
				t.Fatalf("normal report after independent recovery failed: %v", err)
			}
		})
	}
}
