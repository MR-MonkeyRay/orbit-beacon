package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestExplicitTicketIsNeverReplacedByCredentialRecovery(t *testing.T) {
	for _, beganRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "before first run", true: "after beginning run"}[beganRun], func(t *testing.T) {
			ctx := context.Background()
			f := newAgentHTTP(t, 1)
			state, cfg, _ := clientState(t)
			node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "explicit ticket"})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := state.Current()
			// Server committed, but no enrollment/report response reached the Agent.
			if _, err := f.enrollments.Enroll(ctx, initial.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: before.InstallationID, NodeToken: before.NodeToken}); err != nil {
				t.Fatal(err)
			}
			if beganRun {
				if _, err := state.BeginRun(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			https := httptest.NewTLSServer(f.handler)
			defer https.Close()
			client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: state, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			for _, invalid := range []string{"invalid-explicit-ticket", "invalid\nexplicit-ticket"} {
				if _, err := client.Enroll(ctx, invalid); err == nil {
					t.Fatal("invalid explicit ticket was silently replaced by node recovery")
				}
				if state.Current().NodeID != "" || state.Current().NodeToken != before.NodeToken {
					t.Fatal("rejected explicit ticket changed the saved identity")
				}
			}
			fresh, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !beganRun {
				oldCheck, err := json.Marshal(v1.EnrollmentCheckRequest{ProtocolVersion: 1, InstallationID: before.InstallationID, NodeToken: before.NodeToken})
				if err != nil {
					t.Fatal(err)
				}
				apiError(t, agentRequest(t, f, "/api/agent/v1/enroll/check", fresh.Token, string(oldCheck)), http.StatusUpgradeRequired, "protocol_unsupported")
				unused, err := f.enrollments.Get(ctx, fresh.ID)
				if err != nil || unused.Uses != 0 {
					t.Fatal("unsupported old check protocol consumed its ticket")
				}
			}
			if _, err := client.Enroll(ctx, fresh.Token); err != nil {
				t.Fatal(err)
			}
			after := state.Current()
			if after.NodeToken == before.NodeToken || after.NodeID != node.ID || after.PendingNodeToken != "" {
				t.Fatal("fresh explicit ticket did not perform credential replacement")
			}
			stored, err := f.enrollments.Get(ctx, fresh.ID)
			if err != nil || stored.Uses != 1 {
				t.Fatalf("explicit ticket reported success without consuming its claim: uses=%d err=%v", stored.Uses, err)
			}
			if _, err := client.Enroll(ctx, ""); err != nil {
				t.Fatalf("ticketless recovery regressed: %v", err)
			}
			if state.Current() != after {
				t.Fatal("ticketless recovery rotated a credential")
			}
		})
	}
}
