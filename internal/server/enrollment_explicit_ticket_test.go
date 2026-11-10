package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
			state, _ := clientState(t)
			node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "explicit ticket"})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := state.Current()
			// 服务器已提交，但注册/报告响应没有到达 Agent。
			if _, err := f.enrollments.Enroll(ctx, initial.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: before.InstallationID, NodeToken: before.NodeToken}); err != nil {
				t.Fatal(err)
			}
			if beganRun {
				if _, err := state.BeginRun(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			var recoveries atomic.Int64
			https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/agent/v1/recover" {
					recoveries.Add(1)
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer https.Close()
			client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: state})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			for _, invalid := range []string{"invalid-explicit-ticket", "invalid\nexplicit-ticket"} {
				_, err := client.Enroll(ctx, invalid)
				if err == nil {
					t.Fatal("invalid explicit ticket was silently replaced by node recovery")
				}
				if invalid == "invalid-explicit-ticket" {
					var rejected *service.HTTPError
					if !errors.As(err, &rejected) || rejected.StatusCode != http.StatusForbidden {
						t.Fatalf("invalid explicit ticket did not preserve 403: %v", err)
					}
				}
				if state.Current().NodeID != "" || state.Current().NodeToken != before.NodeToken {
					t.Fatal("rejected explicit ticket changed the saved identity")
				}
			}
			if recoveries.Load() != 0 {
				t.Fatal("invalid explicit ticket invoked credential recovery")
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
