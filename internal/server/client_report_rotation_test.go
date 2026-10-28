package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestNormalReportRecoversDurableRotationAfterUnauthorized(t *testing.T) {
	for _, outcome := range []string{"same-process pending success", "pending success", "already promoted", "pending unauthorized", "pending unavailable", "pending response lost", "missing node ID", "different node ID"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			f := newAgentHTTP(t, 1)
			state, path := clientState(t)
			run, err := state.BeginRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "normal report rotation"})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var recovering bool
			var candidate string
			var credentials []string
			var reports []v1.Report
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				isRecovery, pending := recovering, candidate
				mu.Unlock()
				if isRecovery {
					if r.URL.Path != "/api/agent/v1/report" || r.Method != http.MethodPost {
						t.Error("normal report recovery used an enrollment or non-report request")
					}
					var report v1.Report
					if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					mu.Lock()
					credentials = append(credentials, r.Header.Get("Authorization"))
					reports = append(reports, report)
					mu.Unlock()
					if r.Header.Get("Authorization") == "Bearer "+pending {
						durable, err := identity.Open(t.Context(), path, identity.Options{})
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if outcome != "already promoted" && durable.Current().PendingNodeToken != pending && report.Seq == 1 {
							t.Error("pending token promoted before authenticated success")
						}
						switch outcome {
						case "pending unauthorized":
							w.WriteHeader(401)
							return
						case "pending unavailable":
							w.WriteHeader(503)
							return
						case "pending response lost":
							connection, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							_ = connection.Close()
							return
						case "missing node ID", "different node ID":
							id := ""
							if outcome == "different node ID" {
								id = "another-node"
							}
							_ = json.NewEncoder(w).Encode(v1.ReportResponse{NodeID: id, AcceptedSeq: report.Seq})
							return
						}
					}
					// Decode only to inspect the retry; preserve the request for the real handler.
					data, err := json.Marshal(report)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(data))
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			dropper := &enrollmentResponseDropper{base: server.Client().Transport}
			rotator, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: &http.Client{Transport: dropper}, Identity: state})
			if err != nil {
				t.Fatal(err)
			}
			defer rotator.Close()
			if _, err := rotator.Enroll(ctx, initial.Token); err != nil {
				t.Fatal(err)
			}
			// Independent stores model a running process whose in-memory identity is stale.
			stale, err := identity.Open(t.Context(), path, identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "same-process pending success" {
				stale = state
			}
			client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: stale})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			before := stale.Current()
			rotation, err := f.enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			dropper.drop = true
			if _, err := rotator.Enroll(ctx, rotation.Token); err == nil {
				t.Fatal("rotation response loss unexpectedly succeeded")
			}
			staged := state.Current()
			if staged.NodeToken != before.NodeToken || staged.PendingNodeToken == "" || staged.PendingNodeToken == before.NodeToken {
				t.Fatal("rotation did not leave distinct durable credentials")
			}
			if bound, err := f.nodes.AuthenticateToken(ctx, staged.PendingNodeToken); err != nil || bound.ID != node.ID {
				t.Fatalf("rotation did not commit: %v", err)
			}
			if outcome == "already promoted" {
				if err := state.ConfirmEnrollment(t.Context(), staged.PendingNodeToken, node.ID); err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			candidate, recovering = staged.PendingNodeToken, true
			mu.Unlock()
			var report v1.Report
			if err := json.Unmarshal([]byte(reportBody(run.RunGeneration, 1, run.SessionID, 10)), &report); err != nil {
				t.Fatal(err)
			}
			response, err := client.Report(ctx, report)
			success := outcome == "same-process pending success" || outcome == "pending success" || outcome == "already promoted"
			if success && (err != nil || response.NodeID != node.ID) {
				t.Errorf("normal report did not recover durable credential: response=%+v err=%v", response, err)
			}
			if !success && err == nil {
				t.Error("failed or unbound pending retry reported success")
			}
			mu.Lock()
			gotCredentials := append([]string(nil), credentials...)
			gotReports := append([]v1.Report(nil), reports...)
			mu.Unlock()
			want := []string{"Bearer " + before.NodeToken, "Bearer " + staged.PendingNodeToken}
			if !reflect.DeepEqual(gotCredentials, want) {
				t.Errorf("401 recovery must retry exactly once with a distinct durable credential: got=%v want=%v", gotCredentials, want)
			}
			if len(gotReports) == 2 && !reflect.DeepEqual(gotReports[0], gotReports[1]) {
				t.Error("credential retry changed the report payload, ownership, or sequence")
			}
			durable, err := identity.Open(t.Context(), path, identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			after := durable.Current()
			if after.InstallationID != before.InstallationID || after.RunGeneration != before.RunGeneration || after.SessionID != before.SessionID || after.NodeID != before.NodeID {
				t.Error("report credential recovery changed identity or run ownership")
			}
			if !success {
				if after != staged {
					t.Error("failed pending retry changed active or pending durable identity")
				}
				return
			}
			if after.NodeToken != staged.PendingNodeToken || after.PendingNodeToken != "" {
				t.Error("successful retry did not durably promote candidate")
			}
			report.Seq++
			if _, err := client.Report(ctx, report); err != nil {
				t.Fatalf("future report failed after promotion: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(credentials) != 3 || credentials[2] != "Bearer "+staged.PendingNodeToken {
				t.Errorf("future report did not use promoted token directly: %v", credentials)
			}
		})
	}
}
