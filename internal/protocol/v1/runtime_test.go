package v1_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestReportRequiresValidRuntime(t *testing.T) {
	var report map[string]json.RawMessage
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"", `null`, `{"report_interval_seconds":1,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[]}`} {
		t.Run(runtime, func(t *testing.T) {
			if runtime == "" {
				delete(report, "runtime")
			} else {
				report["runtime"] = json.RawMessage(runtime)
			}
			body, _ := json.Marshal(report)
			var value v1.Report
			if err := json.Unmarshal(body, &value); err == nil && value.Validate() == nil {
				t.Fatal("accepted report without valid runtime")
			}
		})
	}
}

func TestReportRuntimeRequiresEveryNonNullField(t *testing.T) {
	for _, field := range []string{"report_interval_seconds", "disk_interval_seconds", "interfaces", "disk_paths", "probe_targets"} {
		for _, missing := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/missing=%t", field, missing), func(t *testing.T) {
				var report map[string]json.RawMessage
				if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
					t.Fatal(err)
				}
				var runtime map[string]json.RawMessage
				if err := json.Unmarshal(report["runtime"], &runtime); err != nil {
					t.Fatal(err)
				}
				if missing {
					delete(runtime, field)
				} else {
					runtime[field] = json.RawMessage(`null`)
				}
				report["runtime"], _ = json.Marshal(runtime)
				body, _ := json.Marshal(report)
				var decoded v1.Report
				if err := json.Unmarshal(body, &decoded); err == nil && decoded.Validate() == nil {
					t.Fatal("incomplete runtime accepted")
				}
			})
		}
	}
}

func TestReportProbeTargetsFollowItsRuntimeBoundary(t *testing.T) {
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	target := "https://example.com/health"
	latency := 1.0
	report.ProbeResults = []v1.ProbeResult{{Target: target, OK: true, LatencyMS: &latency, CheckedAt: report.SampledAt}}
	if err := report.Validate(); err == nil {
		t.Fatal("accepted a probe result removed from this report's runtime")
	}
	report.Runtime.ProbeTargets = []string{target}
	if err := report.Validate(); err != nil {
		t.Fatalf("configured probe result rejected: %v", err)
	}
}

func TestMaximumAdmittedRuntimeReportFitsWireLimit(t *testing.T) {
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	runtime := v1.DefaultRuntimeSettings()
	runtime.Interfaces, runtime.DiskPaths = nil, nil
	for i := range 8 {
		runtime.Interfaces = append(runtime.Interfaces, strings.Repeat("a", 254)+fmt.Sprint(i))
		runtime.DiskPaths = append(runtime.DiskPaths, "/"+strings.Repeat("p", 1022)+fmt.Sprint(i))
	}
	if err := runtime.Validate(); err != nil {
		t.Fatal(err)
	}
	report.Runtime = runtime
	report.Metrics.Disks, report.Metrics.Networks = nil, nil
	delete(report.Metrics.MetricStatus, "disk:/")
	delete(report.Metrics.MetricStatus, "network:eth0")
	delete(report.Metrics.Sources, "disk:/")
	delete(report.Metrics.Sources, "network:eth0")
	total, used, rate, window := uint64(8192), uint64(4096), 1.0, int64(10000)
	for _, path := range runtime.DiskPaths {
		report.Metrics.Disks = append(report.Metrics.Disks, v1.DiskMetrics{Path: path, TotalBytes: &total, UsedBytes: &used, SampledAt: report.SampledAt})
		key := v1.DiskMetricKey(path)
		report.Metrics.MetricStatus[key] = "ok"
		report.Metrics.Sources[key] = v1.SourceMetadata{SampleID: strings.Repeat("d", 255), SourceIntervalMS: 60000}
	}
	for _, name := range runtime.Interfaces {
		report.Metrics.Networks = append(report.Metrics.Networks, v1.NetworkMetrics{Name: name, RxBytesPerSec: &rate, TxBytesPerSec: &rate})
		key := v1.NetworkMetricKey(name)
		report.Metrics.MetricStatus[key] = "ok"
		report.Metrics.Sources[key] = v1.SourceMetadata{SampleID: strings.Repeat("n", 255), SourceIntervalMS: 10000, MeasurementWindowMS: &window}
	}
	report.Traffic = nil
	for i := range v1.MaxTrafficEntries {
		report.Traffic = append(report.Traffic, v1.Traffic{Epoch: fmt.Sprintf("%064x", i+1), PeriodStart: report.SampledAt.Truncate(time.Hour), RxBytes: "9223372036854775807", TxBytes: "9223372036854775807"})
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > v1.MaxMessageBytes {
		t.Fatalf("admitted runtime generated %d-byte report", len(encoded))
	}
}

func TestDecodeRuntimeSettingsPreservesSelectionShape(t *testing.T) {
	for _, selection := range []string{"[]", "null"} {
		raw := fmt.Sprintf(`{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":%s,"disk_paths":%s,"probe_targets":%s}`, selection, selection, selection)
		runtime, err := v1.DecodeRuntimeSettings(raw)
		if err != nil {
			t.Fatal(err)
		}
		wantNil := selection == "null"
		if (runtime.Interfaces == nil) != wantNil || (runtime.DiskPaths == nil) != wantNil || (runtime.ProbeTargets == nil) != wantNil {
			t.Fatalf("selection=%s runtime=%+v", selection, runtime)
		}
	}
	for _, raw := range []string{"", "null", "{", `{"report_interval_seconds":1,"disk_interval_seconds":60}`} {
		if _, err := v1.DecodeRuntimeSettings(raw); err == nil {
			t.Fatalf("accepted invalid runtime %q", raw)
		}
	}
}
