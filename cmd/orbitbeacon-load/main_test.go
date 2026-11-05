package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestReportsMatchProtocol(t *testing.T) {
	for _, count := range []int{0, 1, 8} {
		n, err := newNode(0)
		if err != nil {
			t.Fatal(err)
		}
		enrollment := v1.EnrollmentRequest{ProtocolVersion: v1.ProtocolVersion, InstallationID: n.installation, NodeToken: n.token, System: n.system}
		if err := enrollment.Validate(); err != nil {
			t.Fatal(err)
		}
		report := makeReport(options{interval: 10 * time.Second, disks: count, networks: count, probes: 5}, n, 1, time.Now().UTC())
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > v1.MaxMessageBytes {
			t.Fatalf("report too large: %d", len(data))
		}
		var decoded v1.Report
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := decoded.Validate(); err != nil {
			t.Fatal(err)
		}
		if len(decoded.Metrics.Disks) != count || len(decoded.Metrics.Networks) != count || len(decoded.ProbeResults) != 5 {
			t.Fatal("wrong series count")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPOutcomesWithoutSocket(t *testing.T) {
	stats := &summary{outcomes: make(map[string]int)}
	for _, code := range []string{"report_busy", "storage_busy"} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/api/agent/v1/report" {
				t.Fatal("wrong request")
			}
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"code":"` + code + `"}`)), Header: make(http.Header)}, nil
		})}
		result, _, err := post(context.Background(), client, "http://beacon.test/api/agent/v1/report", "secret", struct{}{})
		if err != nil {
			t.Fatal(err)
		}
		stats.record(result)
	}
	stats.record(outcome{status: 200, elapsed: time.Millisecond})
	stats.record(outcome{})
	if stats.outcomes["503 report_busy"] != 1 || stats.outcomes["503"] != 1 || stats.outcomes["transport_error"] != 1 || stats.success != 1 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestPercentile(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Millisecond
	}
	if percentile(nil, 0.99) != 0 || percentile(samples, 0.50) != 50*time.Millisecond || percentile(samples, 0.95) != 95*time.Millisecond || percentile(samples, 0.99) != 99*time.Millisecond {
		t.Fatal("incorrect percentile")
	}
}

func TestSendReportsOmitsCanceledInFlightReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := newNode(0)
	if err != nil {
		t.Fatal(err)
	}
	stats := &summary{outcomes: make(map[string]int)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	})}
	sendReports(ctx, client, options{baseURL: "http://beacon.test", interval: 10 * time.Second}, n, time.Now(), stats)
	if len(stats.latencies) != 0 || len(stats.outcomes) != 0 {
		t.Fatalf("canceled report counted against the server: %+v", stats)
	}
}
