package collect_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/agent/collect"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestMaximumOrdinarySelectionsProduceSendableReport(t *testing.T) {
	config := v1.DesiredConfig{Revision: 1, ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, BillingDay: 1, QuotaBytes: "0"}
	base := os.TempDir() + string(filepath.Separator)
	for i := range 8 {
		config.DiskPaths = append(config.DiskPaths, base+strings.Repeat("a", 1023-len(base))+string(rune('a'+i)))
		config.Interfaces = append(config.Interfaces, strings.Repeat("a", 254)+string(rune('a'+i)))
	}
	for i := range 5 {
		prefix := "https://probe" + strconv.Itoa(i) + ".example/"
		config.ProbeTargets = append(config.ProbeTargets, prefix+strings.Repeat("a", 1024-len(prefix)))
	}
	collector, err := collect.New(collect.Options{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	metrics, _ := collector.Collect(ctx)
	report := v1.Report{ProtocolVersion: 1, RunGeneration: 1, SessionID: "report-size-boundary", Seq: 1, SampledAt: time.Now().UTC(), ConfigRevision: 1, Metrics: metrics, Traffic: []v1.Traffic{}, System: &v1.System{Hostname: strings.Repeat("<", 255), OS: strings.Repeat("&", 64), Arch: strings.Repeat(">", 64)}}
	for i := range 24 {
		report.Traffic = append(report.Traffic, v1.Traffic{Epoch: strings.Repeat("f", 64), PeriodStart: time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(i) * time.Hour), RxBytes: "9223372036854775807", TxBytes: "9223372036854775807"})
	}
	for _, target := range config.ProbeTargets {
		latency, code := 59999.999999, 200
		report.ProbeResults = append(report.ProbeResults, v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, StatusCode: &code, CheckedAt: time.Now().UTC()})
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("selected series lost report validity: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > v1.MaxMessageBytes {
		t.Fatalf("accepted ordinary selections produce %d-byte report", len(data))
	}
}
