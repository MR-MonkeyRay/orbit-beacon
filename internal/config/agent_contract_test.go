package config_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

const reportJSON = `{
 "protocol_version":1,"run_generation":7,"session_id":"run-seven","seq":9,
 "sampled_at":"2026-10-26T12:00:00Z",
 "system":{"hostname":"same-hostname","os":"linux","arch":"amd64"},
 "metrics":{"disk_io":null,"pressure":null,"temperature_celsius":null,
  "cpu_percent":null,"load1":0.5,"load5":1.25,"load15":2,
  "memory_total_bytes":8192,"memory_available_bytes":4096,
  "swap_total_bytes":0,"swap_used_bytes":0,"uptime_seconds":123,
  "disks":[{"path":"/","total_bytes":8192,"used_bytes":2048,"inodes_total":null,"inodes_used":null,"sampled_at":"2026-10-26T12:00:00Z"}],
  "networks":[{"name":"eth0","rx_bytes_per_sec":null,"tx_bytes_per_sec":null}],
  "metric_status":{"cpu_percent":"stale","load1":"ok","load5":"ok","load15":"ok","memory_total_bytes":"ok","memory_available_bytes":"ok","swap_total_bytes":"ok","swap_used_bytes":"ok","uptime_seconds":"ok","disk:/":"ok","network:eth0":"stale"},
  "sources":{"cpu":{"sample_id":"cpu-1","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":0},"load":{"sample_id":"load-1","age_ms":0,"source_interval_ms":10000},"memory":{"sample_id":"memory-1","age_ms":0,"source_interval_ms":10000},"swap":{"sample_id":"swap-1","age_ms":0,"source_interval_ms":10000},"uptime":{"sample_id":"uptime-1","age_ms":0,"source_interval_ms":10000},"disk:/":{"sample_id":"disk-1","age_ms":10000,"source_interval_ms":60000},"network:eth0":{"sample_id":"net-1","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":0}}
 },
	"traffic":[{"epoch":"deadbeef","period_start":"2026-10-01T00:00:00Z","rx_bytes":"9223372036854775807","tx_bytes":"9007199254740993"}],
 "probe_results":[]
}`

func TestMaximumAdmittedRuntimeReportFitsWireLimit(t *testing.T) {
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	runtime := config.DefaultRuntimeSettings()
	runtime.Interfaces, runtime.DiskPaths = nil, nil
	for i := range 8 {
		runtime.Interfaces = append(runtime.Interfaces, strings.Repeat("a", 254)+fmt.Sprint(i))
		runtime.DiskPaths = append(runtime.DiskPaths, "/"+strings.Repeat("p", 1022)+fmt.Sprint(i))
	}
	if err := runtime.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, length := range []int{1024, 1024, 1024, 884, 54} {
		prefix := fmt.Sprintf("https://probe%d.example/", i)
		target := prefix + strings.Repeat("a", length-len(prefix))
		runtime.ProbeTargets = append(runtime.ProbeTargets, target)
		latency, code := 0.00010000000000000002, 599
		expiry := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("widest", 23*3600+59*60))
		report.ProbeResults = append(report.ProbeResults, v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, StatusCode: &code, CheckedAt: report.SampledAt, TLSExpiresAt: &expiry})
	}
	if err := runtime.Validate(); err != nil {
		t.Fatal(err)
	}
	runtime.DisabledFeatures = v1.DisabledFeatureOrder()
	widestRate, widestPercent, widestTemperature := 999999999999999.9, 99.99, 149.99
	report.Metrics.DiskIO = &v1.DiskIOMetrics{ReadBytesPerSec: widestRate, WriteBytesPerSec: widestRate, BusyPercent: widestPercent}
	report.Metrics.Pressure = &v1.PressureMetrics{CPU: &widestPercent, Memory: &widestPercent, IO: &widestPercent}
	report.Metrics.TemperatureCelsius = &widestTemperature

	report.Metrics.Disks, report.Metrics.Networks = nil, nil
	delete(report.Metrics.MetricStatus, "disk:/")
	delete(report.Metrics.MetricStatus, "network:eth0")
	delete(report.Metrics.Sources, "disk:/")
	delete(report.Metrics.Sources, "network:eth0")
	total, used, rate, window := uint64(8192), uint64(4096), 1.0, int64(10000)
	for _, path := range runtime.DiskPaths {
		report.Metrics.Disks = append(report.Metrics.Disks, v1.DiskMetrics{Path: path, TotalBytes: &total, UsedBytes: &used, InodesTotal: &total, InodesUsed: &used, SampledAt: report.SampledAt})
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
	t.Logf("admitted report: %d bytes; headroom: %d bytes", len(encoded), v1.MaxMessageBytes-len(encoded))
	if len(encoded) > v1.MaxMessageBytes {
		t.Fatalf("admitted runtime generated %d-byte report", len(encoded))
	}
}

func TestRuntimeCapabilityEnumsAndCommandPermission(t *testing.T) {
	defaults := config.DefaultRuntimeSettings()
	for _, name := range v1.RemoteCommandOrder() {
		if !defaults.AllowsCommand(name) {
			t.Fatalf("default disallows %s", name)
		}
	}
	if defaults.AllowsCommand("shell") {
		t.Fatal("unknown command allowed")
	}
	for _, field := range []string{"remote_commands", "disabled_features"} {
		for _, values := range [][]string{{"unknown"}, {"restart", "restart"}, {"disk", "disk"}} {
			settings := config.DefaultRuntimeSettings()
			if field == "remote_commands" {
				settings.RemoteCommands = values
			} else {
				settings.DisabledFeatures = values
			}
			if settings.Validate() == nil {
				t.Fatalf("accepted %s=%v", field, values)
			}
		}
	}
	empty := defaults
	empty.RemoteCommands = []string{}
	if empty.AllowsCommand("restart") || empty.Equal(defaults) {
		t.Fatal("empty permission fell back to defaults")
	}
	for _, mutate := range []func(*config.RuntimeSettings){
		func(c *config.RuntimeSettings) { c.ReportIntervalSeconds++ },
		func(c *config.RuntimeSettings) { c.DiskIntervalSeconds++ },
		func(c *config.RuntimeSettings) { c.Interfaces = []string{"eth0"} },
		func(c *config.RuntimeSettings) { c.DiskPaths = []string{"/"} },
		func(c *config.RuntimeSettings) { c.ProbeTargets = []string{"tcp://example.test:80"} },
		func(c *config.RuntimeSettings) { c.RemoteCommands = []string{} },
		func(c *config.RuntimeSettings) { c.DisabledFeatures = []string{v1.FeatureDisk} },
	} {
		other := config.DefaultRuntimeSettings()
		mutate(&other)
		if defaults.Equal(other) {
			t.Fatal("Equal missed changed field")
		}
	}
}

func TestRuntimeSettingsProbeTargetValidation(t *testing.T) {
	valid := config.RuntimeSettings{
		ReportIntervalSeconds: 30, DiskIntervalSeconds: 60,
		ProbeTargets: []string{"tcp://db:5432", "https://example.com/health"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid probe targets rejected: %v", err)
	}
	duplicate := valid
	duplicate.ProbeTargets = []string{"tcp://db:5432", "tcp://db:5432"}
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate probe targets accepted")
	}
	tooMany := valid
	tooMany.ProbeTargets = []string{"icmp://host0", "icmp://host1", "icmp://host2", "icmp://host3", "icmp://host4", "icmp://host5"}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("too many probe targets accepted")
	}
}

func TestSelectionRejectsAggregateReportExpansion(t *testing.T) {
	for _, character := range []string{"<", "&", "\x01", "\u2028", "\\"} {
		t.Run(character, func(t *testing.T) {
			paths := make([]string, 8)
			interfaces := make([]string, 8)
			for i := range paths {
				paths[i] = "/" + strings.Repeat(character, (1020/len(character))) + string(rune('a'+i))
				interfaces[i] = strings.Repeat("<", 254) + string(rune('a'+i))
			}
			if err := (config.RuntimeSettings{DiskPaths: paths, Interfaces: interfaces}).ValidateSelection(); err == nil {
				t.Fatal("selection can expand beyond the report budget")
			}
		})
	}
	paths, interfaces := make([]string, 8), make([]string, 8)
	for i := range paths {
		paths[i] = "/" + strings.Repeat("a", 1022) + string(rune('a'+i))
		interfaces[i] = strings.Repeat("a", 254) + string(rune('a'+i))
	}
	if err := (config.RuntimeSettings{DiskPaths: paths, Interfaces: interfaces}).ValidateSelection(); err != nil {
		t.Fatalf("ordinary maximum-length selections rejected: %v", err)
	}
	probes := make([]string, 5)
	for i := range probes {
		suffix := "?id=" + strconv.Itoa(i)
		probes[i] = "https://example.com/" + strings.Repeat("<", 1024-len("https://example.com/")-len(suffix)) + suffix
	}
	if err := (config.RuntimeSettings{DiskPaths: paths, Interfaces: interfaces, ProbeTargets: probes}).ValidateSelection(); err == nil {
		t.Fatal("probe results can expand beyond the report budget")
	}
	probeHeavy := config.RuntimeSettings{DiskPaths: paths, ProbeTargets: probes}
	if err := probeHeavy.ValidateSelection(); err != nil {
		if validation, ok := err.(*config.ValidationError); !ok || validation.Fields["probe_targets"] == "" {
			t.Fatalf("probe target budget error missing probe_targets field: %v", err)
		}
	}
}

func TestHostFeatureImplications(t *testing.T) {
	cfg := config.DefaultRuntimeSettings()
	cfg.DisabledFeatures = []string{v1.FeatureDisk}
	if !cfg.FeatureDisabled(v1.FeatureDiskIO) || cfg.FeatureDisabled(v1.FeaturePressure) {
		t.Fatal("disk implication")
	}
	cfg.DisabledFeatures = []string{v1.FeatureDiskIO}
	if cfg.FeatureDisabled(v1.FeatureDisk) {
		t.Fatal("reverse implication")
	}
	cfg.DisabledFeatures = []string{v1.FeatureNetwork}
	if !cfg.FeatureDisabled(v1.FeatureTraffic) {
		t.Fatal("network implication")
	}
}
