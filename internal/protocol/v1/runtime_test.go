package v1_test

import (
	"encoding/json"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestReportHasNoRuntime(t *testing.T) {
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["runtime"]; exists {
		t.Fatalf("runtime on wire: %s", body)
	}
}
func TestPendingProbeValidation(t *testing.T) {
	pending := v1.ProbeResult{Target: "https://example.com", Error: "pending"}
	if err := pending.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*v1.ProbeResult){func(p *v1.ProbeResult) { p.OK = true }, func(p *v1.ProbeResult) { p.CheckedAt = time.Now() }, func(p *v1.ProbeResult) { n := 1.0; p.LatencyMS = &n }, func(p *v1.ProbeResult) { n := 200; p.StatusCode = &n }, func(p *v1.ProbeResult) { n := time.Now(); p.TLSExpiresAt = &n }} {
		p := pending
		mutate(&p)
		if p.Validate() == nil {
			t.Fatalf("invalid pending accepted: %+v", p)
		}
	}
}
func TestReportResponseContainsOnlyAcknowledgements(t *testing.T) {
	body, err := json.Marshal(v1.ReportResponse{Command: &v1.Command{ID: "00000000-0000-4000-8000-000000000001", Kind: "restart", ExpiresAt: time.Now()}, NodeID: "node", AcceptedSeq: 1, ServerTime: time.Now(), TrafficAcks: []v1.Traffic{{RxBytes: "0", TxBytes: "0"}}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"node_id": true, "accepted_seq": true, "server_time": true, "traffic_acks": true, "command": true}
	for name := range fields {
		if !allowed[name] {
			t.Fatalf("response contains setting %s", name)
		}
	}
	if len(fields) != len(allowed) {
		t.Fatalf("response fields: %s", body)
	}
}

func TestDisabledCollectorStatusesSurviveWireValidation(t *testing.T) {
	for _, key := range v1.DisabledStatusFields() {
		t.Run(key, func(t *testing.T) {
			var report v1.Report
			if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
				t.Fatal(err)
			}
			report.Metrics.MetricStatus[key] = "disabled"
			body, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded v1.Report
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := decoded.Validate(); err != nil {
				t.Fatal(err)
			}
			if decoded.Metrics.MetricStatus[key] != "disabled" {
				t.Fatal("disabled signal dropped")
			}
			decoded.Metrics.MetricStatus[key] = "ok"
			if decoded.Validate() == nil {
				t.Fatal("collector status must only signal disabled")
			}
		})
	}
}
