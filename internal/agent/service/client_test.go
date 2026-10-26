package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestEnrollmentPersistsIdentityBeforeRequestAndReusesItAfterLostResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "desired.json")
	cfg, err := service.OpenConfigStore(configPath, desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	const shortLivedToken = "short-lived-enrollment-token-never-saved"
	var mu sync.Mutex
	var requests []v1.EnrollmentRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/v1/enroll/check" {
			_, _ = w.Write([]byte(`{"claimed":false,"credential_state":"unbound","server_time":"2026-10-26T12:00:00Z"}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/agent/v1/enroll" || r.URL.RawQuery != "" {
			t.Errorf("wrong enrollment request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+shortLivedToken {
			t.Error("enrollment did not use short-lived bearer")
		}
		var request v1.EnrollmentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Error("enrollment sent before identity was durable")
			w.WriteHeader(500)
			return
		}
		var persisted identity.Identity
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Error(err)
		}
		if persisted.InstallationID != request.InstallationID || persisted.NodeToken != request.NodeToken {
			t.Error("request identity differs from preexisting disk identity")
		}
		mu.Lock()
		requests = append(requests, request)
		attempt := len(requests)
		mu.Unlock()
		if attempt == 1 {
			// The binding succeeded but the response is lost at the transport layer.
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_id":"bound-node","server_time":"2026-10-26T12:00:00Z"}`))
	}))
	defer server.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Enroll(context.Background(), shortLivedToken); err == nil {
		t.Fatal("lost response reported as enrollment success")
	}
	reopened, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	client, err = service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: reopened, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Enroll(context.Background(), shortLivedToken); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[0].InstallationID != requests[1].InstallationID || requests[0].NodeToken != requests[1].NodeToken || requests[0].ProtocolVersion != 1 {
		t.Fatal("retry changed claim identity")
	}
	for _, file := range []string{path, configPath} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), shortLivedToken) {
			t.Fatal("short-lived enrollment token persisted")
		}
	}
	if reopened.Current().RunGeneration != 0 {
		t.Fatal("enroll-only invocation incremented run_generation")
	}
}

func TestReportRecoveryUsesNodeTokenAndAcknowledgesOnlyAppliedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := state.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := service.OpenConfigStore(filepath.Join(t.TempDir(), "desired.json"), desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []map[string]json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agent/v1/report" || r.Header.Get("Authorization") != "Bearer "+run.NodeToken {
			t.Errorf("recovery/report not authenticated as node: %s %s", r.Method, r.URL)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		for _, forbidden := range []string{"node_id", "gid", "node_token", "enrollment_token"} {
			if _, exists := body[forbidden]; exists {
				t.Errorf("report sends %s", forbidden)
			}
		}
		mu.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch attempt {
		case 1:
			_, _ = w.Write([]byte(`{"node_id":"00000000-0000-4000-8000-000000000123","accepted_seq":1,"server_time":"2026-10-26T12:00:00Z","desired_config":{"revision":2,"report_interval_seconds":30,"disk_interval_seconds":60,"interfaces":["eth0"],"disk_paths":["/"],"probe_targets":[],"billing_day":1,"quota_bytes":"0"}}`))
		case 2:
			_, _ = w.Write([]byte(`{"node_id":"00000000-0000-4000-8000-000000000123","accepted_seq":2,"server_time":"2026-10-26T12:00:00Z","desired_config":{"revision":3,"report_interval_seconds":1,"disk_interval_seconds":60,"interfaces":["eth0"],"disk_paths":["/"],"probe_targets":[],"billing_day":1,"quota_bytes":"0"}}`))
		default:
			_, _ = w.Write([]byte(`{"node_id":"00000000-0000-4000-8000-000000000123","accepted_seq":3,"server_time":"2026-10-26T12:00:00Z"}`))
		}
	}))
	defer server.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		_, err := client.Report(context.Background(), v1.Report{ProtocolVersion: 1, RunGeneration: run.RunGeneration, SessionID: run.SessionID, Seq: seq, Metrics: schedulerMetrics()})
		if seq != 2 && err != nil {
			t.Fatal(err)
		}
	}
	if cfg.Current().Revision != 2 || cfg.Current().ReportIntervalSeconds != 30 {
		t.Fatal("invalid remote config replaced working settings")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("report recovery performed additional enrollment/requests: %d", len(bodies))
	}
	for i, expected := range []int64{1, 2, 2} {
		var revision int64
		if err := json.Unmarshal(bodies[i]["config_revision"], &revision); err != nil || revision != expected {
			t.Fatalf("report %d acknowledged revision=%d want=%d err=%v", i, revision, expected, err)
		}
	}
	var failure struct {
		Revision int64  `json:"revision"`
		Code     string `json:"code"`
	}
	if err := json.Unmarshal(bodies[2]["config_error"], &failure); err != nil || failure.Revision != 3 || failure.Code != "invalid_config" {
		t.Fatalf("rejected config missing next-report error: %+v %v", failure, err)
	}
	if state.Current().RunGeneration != 1 {
		t.Fatal("HTTP retries/recovery changed generation")
	}
}
