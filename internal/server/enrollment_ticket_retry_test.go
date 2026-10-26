package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	"orbitbeacon/internal/server/nodes"
)

func TestOriginalTicketRetryAfterReportPromotionKeepsExactCredential(t *testing.T) {
	for _, name := range []string{"durable association", "legacy identity without association", "legacy active receipt with unrelated pending"} {
		legacy := name != "durable association"
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newAgentHTTP(t, 1)
			node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "ticket retry"})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			state, cfg, path := clientState(t)
			https := httptest.NewTLSServer(f.handler)
			defer https.Close()
			dropper := &enrollmentResponseDropper{base: https.Client().Transport}
			client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: &http.Client{Transport: dropper}, Identity: state, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Enroll(ctx, initial.Token); err != nil {
				t.Fatal(err)
			}
			run, err := state.BeginRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ticket, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			dropper.drop = true
			if _, err := client.Enroll(ctx, ticket.Token); err == nil {
				t.Fatal("lost response was acknowledged")
			}
			candidate := state.Current().PendingNodeToken
			if _, err := client.Report(ctx, mustReport(t, reportBody(run.RunGeneration, 1, run.SessionID, 25))); err != nil {
				t.Fatal(err)
			}
			promoted := state.Current()
			if promoted.NodeToken != candidate || promoted.PendingNodeToken != "" {
				t.Fatal("report did not promote the committed candidate")
			}
			if name == "legacy active receipt with unrelated pending" {
				promoted, _, err = state.StageEnrollment(t.Context(), [32]byte{99}, true)
				if err != nil {
					t.Fatal(err)
				}
			}
			if legacy {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]json.RawMessage
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				delete(record, "enrollment_attempts")
				data, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := identity.Open(path, identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			retry, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: reopened, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer retry.Close()
			if _, err := retry.Enroll(ctx, ticket.Token); err != nil {
				t.Fatalf("original ticket could not confirm its report-promoted credential: %v", err)
			}
			if reopened.Current() != promoted {
				t.Fatal("exact retry invented a candidate or changed the durable run")
			}
			stored, err := f.enrollments.Get(ctx, ticket.ID)
			if err != nil || stored.Uses != 1 {
				t.Fatalf("retry consumed another claim: uses=%d err=%v", stored.Uses, err)
			}
			fresh, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := retry.Enroll(ctx, fresh.Token); err != nil {
				t.Fatal(err)
			}
			newest := reopened.Current()
			if newest.NodeToken == candidate || newest.PendingNodeToken != "" {
				t.Fatal("fresh ticket did not rotate to a new credential")
			}
			if _, err := retry.Enroll(ctx, ticket.Token); err == nil {
				t.Fatal("superseded receipt unexpectedly succeeded")
			}
			if reopened.Current() != newest {
				t.Fatal("superseded ticket staged a spurious candidate")
			}
		})
	}
}
