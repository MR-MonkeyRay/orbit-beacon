package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestReportRecoveryPromotionPreservesConcurrentDurableRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := identity.Open(t.Context(), path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := state.BeginRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfirmEnrollment(t.Context(), run.NodeToken, "same-node"); err != nil {
		t.Fatal(err)
	}
	other, err := identity.Open(t.Context(), path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := other.StageEnrollment(t.Context(), [32]byte{1}, true)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var credentials []string
	var concurrentRun identity.Identity
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer "+run.NodeToken {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+pending.PendingNodeToken {
			t.Error("retry used neither active nor durable pending credential")
			w.WriteHeader(401)
			return
		}
		// A separate process updates the durable generation after credential reload
		// but before the successful response permits pending promotion.
		var err error
		concurrentRun, err = other.BeginRun(t.Context())
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(v1.ReportResponse{NodeID: "same-node", AcceptedSeq: 1})
	}))
	defer server.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Report(context.Background(), v1.Report{ProtocolVersion: 1, RunGeneration: run.RunGeneration, SessionID: run.SessionID, Seq: 1, Runtime: runtimeSettings(10), Metrics: schedulerMetrics()}); err != nil {
		t.Errorf("report did not recover stale in-memory identity: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(credentials, []string{"Bearer " + run.NodeToken, "Bearer " + pending.PendingNodeToken}) {
		t.Errorf("recovery retry was not bounded/distinct: %v", credentials)
	}
	if concurrentRun.RunGeneration == 0 {
		t.Fatal("pending credential was never authenticated")
	}
	durable, err := identity.Open(t.Context(), path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := concurrentRun
	want.NodeToken, want.PendingNodeToken = pending.PendingNodeToken, ""
	if durable.Current() != want {
		t.Errorf("promotion overwrote independently committed identity fields: got=%+v want=%+v", durable.Current(), want)
	}
}

func TestReportDoesNotRetryWithoutDistinctCredentialOrWithoutUnauthorized(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		pending bool
	}{
		{"unauthorized without candidate", http.StatusUnauthorized, false},
		{"unavailable with candidate", http.StatusServiceUnavailable, true},
		{"forbidden with candidate", http.StatusForbidden, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if tt.pending {
				if _, _, err := state.StageEnrollment(t.Context(), [32]byte{1}, true); err != nil {
					t.Fatal(err)
				}
			}
			before := state.Current()
			var mu sync.Mutex
			var calls int
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Report(context.Background(), v1.Report{Seq: 1, Runtime: runtimeSettings(10), Metrics: schedulerMetrics()}); err == nil {
				t.Error("rejected report returned success")
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != 1 {
				t.Errorf("retried without a 401 and distinct durable alternate: %d requests", calls)
			}
			if state.Current() != before {
				t.Error("failed report changed active/pending identity")
			}
		})
	}
}
