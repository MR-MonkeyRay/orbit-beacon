package v1_test

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func assertJSONRoundTrip(t *testing.T, input string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(input), dst); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(dst)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(input), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire contract changed:\n got %s\nwant %s", encoded, input)
	}
}

const reportJSON = `{
 "protocol_version":1,"run_generation":7,"session_id":"run-seven","seq":9,
 "sampled_at":"2026-10-26T12:00:00Z","config_revision":3,
 "system":{"hostname":"same-hostname","os":"linux","arch":"amd64"},
 "metrics":{
  "cpu_percent":null,"load1":0.5,"load5":1.25,"load15":2,
  "memory_total_bytes":8192,"memory_available_bytes":4096,
  "swap_total_bytes":0,"swap_used_bytes":0,"uptime_seconds":123,
  "disks":[{"path":"/","total_bytes":8192,"used_bytes":2048,"sampled_at":"2026-10-26T12:00:00Z"}],
  "networks":[{"name":"eth0","rx_bytes_per_sec":null,"tx_bytes_per_sec":null}],
  "metric_status":{"cpu_percent":"stale","load1":"ok","load5":"ok","load15":"ok","memory_total_bytes":"ok","memory_available_bytes":"ok","swap_total_bytes":"ok","swap_used_bytes":"ok","uptime_seconds":"ok","disk:/":"ok","network:eth0":"stale"},
  "sources":{"cpu":{"sample_id":"cpu-1","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":0},"load":{"sample_id":"load-1","age_ms":0,"source_interval_ms":10000},"memory":{"sample_id":"memory-1","age_ms":0,"source_interval_ms":10000},"swap":{"sample_id":"swap-1","age_ms":0,"source_interval_ms":10000},"uptime":{"sample_id":"uptime-1","age_ms":0,"source_interval_ms":10000},"disk:/":{"sample_id":"disk-1","age_ms":10000,"source_interval_ms":60000},"network:eth0":{"sample_id":"net-1","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":0}}
 },
	"traffic":[{"epoch":"deadbeef","period_start":"2026-10-01T00:00:00Z","rx_bytes":"9223372036854775807","tx_bytes":"9007199254740993"}],
 "probe_results":[]
}`

func TestReportV1PreservesNullsSourcesAndDecimalCounters(t *testing.T) {
	var report v1.Report
	assertJSONRoundTrip(t, reportJSON, &report)
	if err := report.Validate(); err != nil {
		t.Fatalf("valid nullable protocol report rejected: %v", err)
	}
}

func TestEnrollmentAndResponseDTOs(t *testing.T) {
	var enrollment v1.EnrollmentRequest
	assertJSONRoundTrip(t, `{"protocol_version":1,"installation_id":"installation-a","node_token":"node-secret","system":{"hostname":"duplicate","os":"linux","arch":"amd64"}}`, &enrollment)
	var versionedEnrollment v1.EnrollmentRequest
	assertJSONRoundTrip(t, `{"protocol_version":1,"installation_id":"installation-a","node_token":"node-secret","system":{"hostname":"duplicate","os":"linux","arch":"amd64","agent_version":"v1.2.3-rc.1+build_7"}}`, &versionedEnrollment)
	var enrolled v1.EnrollmentResponse
	assertJSONRoundTrip(t, `{"node_id":"server-bound-id","server_time":"2026-10-26T12:00:00Z"}`, &enrolled)
	var response v1.ReportResponse
	assertJSONRoundTrip(t, `{"accepted_seq":9,"server_time":"2026-10-26T12:00:00Z","desired_config":{"revision":4,"report_interval_seconds":30,"disk_interval_seconds":60,"interfaces":["eth0"],"disk_paths":["/"],"probe_targets":[],"billing_day":1,"quota_bytes":"9007199254740993"},"traffic_acks":[{"epoch":"deadbeef","period_start":"2026-10-01T00:00:00Z","rx_bytes":"9223372036854775807","tx_bytes":"9007199254740993"}]}`, &response)
}

func TestSystemAgentVersionValidation(t *testing.T) {
	valid := []string{"", "development", "v1.2.3-rc.1+build_7", strings.Repeat("a", 64)}
	for _, version := range valid {
		t.Run("valid_"+version, func(t *testing.T) {
			if err := (v1.System{AgentVersion: version}).Validate(); err != nil {
				t.Fatalf("valid agent version %q rejected: %v", version, err)
			}
		})
	}

	invalid := []string{"has space", "v1/2", "v1:2", "café", "line\nbreak", strings.Repeat("a", 65)}
	for _, version := range invalid {
		t.Run("invalid_"+version, func(t *testing.T) {
			if err := (v1.System{AgentVersion: version}).Validate(); err == nil {
				t.Fatalf("invalid agent version %q accepted", version)
			}
		})
	}
}

func TestTrafficCountersRejectLossyOrNonCanonicalJSON(t *testing.T) {
	for _, value := range []string{`9007199254740993`, `"-1"`, `"1.5"`, `"1e6"`, `"9223372036854775808"`, `"18446744073709551616"`, `"01"`, `null`} {
		t.Run(value, func(t *testing.T) {
			body := strings.Replace(reportJSON, `"9223372036854775807"`, value, 1)
			var report v1.Report
			if err := json.Unmarshal([]byte(body), &report); err == nil {
				if err := report.Validate(); err == nil {
					t.Fatalf("accepted invalid cumulative counter %s", value)
				}
			}
		})
	}
}

func TestTrafficValidation(t *testing.T) {
	valid := v1.Traffic{
		Epoch:       "0123456789abcdef",
		PeriodStart: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		RxBytes:     "9223372036854775807",
		TxBytes:     "0",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid traffic rejected: %v", err)
	}
	maximumEpoch := valid
	maximumEpoch.Epoch = strings.Repeat("f", 64)
	if err := maximumEpoch.Validate(); err != nil {
		t.Fatalf("maximum-length traffic epoch rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*v1.Traffic)
	}{
		{"empty_epoch", func(traffic *v1.Traffic) { traffic.Epoch = "" }},
		{"long_epoch", func(traffic *v1.Traffic) { traffic.Epoch = strings.Repeat("a", 65) }},
		{"uppercase_epoch", func(traffic *v1.Traffic) { traffic.Epoch = "ABCDEF" }},
		{"non_hex_epoch", func(traffic *v1.Traffic) { traffic.Epoch = "boot-a" }},
		{"non_utc_period", func(traffic *v1.Traffic) {
			traffic.PeriodStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("UTC", 0))
		}},
		{"minute_not_aligned", func(traffic *v1.Traffic) { traffic.PeriodStart = traffic.PeriodStart.Add(time.Minute) }},
		{"second_not_aligned", func(traffic *v1.Traffic) { traffic.PeriodStart = traffic.PeriodStart.Add(time.Second) }},
		{"nanosecond_not_aligned", func(traffic *v1.Traffic) { traffic.PeriodStart = traffic.PeriodStart.Add(time.Nanosecond) }},
		{"rx_exceeds_int64", func(traffic *v1.Traffic) { traffic.RxBytes = "9223372036854775808" }},
		{"tx_exceeds_int64", func(traffic *v1.Traffic) { traffic.TxBytes = "18446744073709551615" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			traffic := valid
			test.mutate(&traffic)
			if err := traffic.Validate(); err == nil {
				t.Fatal("invalid traffic accepted")
			}
		})
	}
}

func TestReportTrafficValidation(t *testing.T) {
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	entry := report.Traffic[0]
	report.Traffic = make([]v1.Traffic, v1.MaxTrafficEntries)
	for i := range report.Traffic {
		report.Traffic[i] = entry
		report.Traffic[i].PeriodStart = entry.PeriodStart.Add(time.Duration(i) * time.Hour)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("maximum traffic entries rejected: %v", err)
	}

	report.Traffic = append(report.Traffic, v1.Traffic{
		Epoch:       entry.Epoch,
		PeriodStart: entry.PeriodStart.Add(v1.MaxTrafficEntries * time.Hour),
		RxBytes:     "0",
		TxBytes:     "0",
	})
	if err := report.Validate(); err == nil {
		t.Fatal("report with too many traffic entries accepted")
	}

	report.Traffic = []v1.Traffic{entry, entry}
	if err := report.Validate(); err == nil {
		t.Fatal("report with duplicate traffic identity accepted")
	}

	duplicatePeriod := entry
	duplicatePeriod.Epoch = "cafebabe"
	report.Traffic = []v1.Traffic{entry, duplicatePeriod}
	if err := report.Validate(); err != nil {
		t.Fatalf("same period in different epochs rejected: %v", err)
	}
}

func TestValidateProbeTarget(t *testing.T) {
	valid := []string{
		"tcp://db:5432", "tcp://example.com.:1", "tcp://192.0.2.10:65535", "tcp://[2001:db8::1]:443",
		"tcp://[2001:0DB8:0:0:0:0:0:1]:443", "http://example.com", "https://example.com/health,ready?value=1",
		"icmp://router", "icmp://192.0.2.1", "icmp://[2001:db8::2]",
		"http://h?x=%zz", "http://h:/", "https://[2001:db8::1]:8443/x", "http://127.0.0.1:8080/health", "https://example.com./",
		"http://xn--a/", "http://h:0/", "https://xn--bcher-kva.example/", "https://example.com:443/a?b=c&d=%41",
	}
	for _, target := range valid {
		t.Run("valid_"+target, func(t *testing.T) {
			if err := v1.ValidateProbeTarget(target); err != nil {
				t.Fatalf("valid target %q rejected: %v", target, err)
			}
		})
	}

	invalid := []string{
		"", "udp://db:53", "tcp://db", "tcp://db:0", "tcp://db:65536", "tcp://db:abc",
		"tcp://999.2.3.4:80", "tcp://bad_host:80", "tcp://2001:db8::1:443",
		"https://", "https:///host", "https://host\\evil", "https://@host", "https://user:pass@example.com/",
		"https://example.com/#section", "https://example.com/#", "icmp://host:7", "icmp://host/path",
		"icmp://bad_host", "icmp://", "tcp://db :5432", "https://example.com/a b", "icmp://host\u0085",
		"https://example.com/\ufeff", "http://host:65536", "http://[bad]/",
		"http://999.1.1.1/", "http://example.test/%zz",
		// Parity with the console validator (web/src/console/pages/node-config-helpers.js).
		"http://127.1/", "http://127.0.0.1./", "http://0177.0.0.1/", "http://2130706433/", "http://0x100000000/", "http://%65xample.com/", "http://%25/", "http://例え.テスト/", "http://\u200d.com/", "http://h^x/", "http://0x7f.1/", "tcp://0x7f.1:80",
		"http://h/\u0000x", "http://h/\u007fx", "http://h?x=\u0001", "http://h:99999/", "http://h:abc/", "http://[::1/", "http://[fe80::1%25eth0]/", "http://[::1]evil/", "http://[::1]evil:80/", "http://[::1].example/",
		"https://example.com/" + strings.Repeat("é", 503),
	}
	for _, target := range invalid {
		t.Run("invalid_"+target, func(t *testing.T) {
			if err := v1.ValidateProbeTarget(target); err == nil {
				t.Fatalf("invalid target %q accepted", target)
			}
		})
	}
	if target := "https://example.com/" + strings.Repeat("é", 502); len(target) != 1024 {
		t.Fatalf("test target is %d bytes, want 1024", len(target))
	} else if err := v1.ValidateProbeTarget(target); err != nil {
		t.Fatalf("1024-byte target rejected: %v", err)
	}
}

func TestDesiredConfigProbeTargetValidation(t *testing.T) {
	valid := v1.DesiredConfig{
		Revision: 1, ReportIntervalSeconds: 30, DiskIntervalSeconds: 60,
		ProbeTargets: []string{"tcp://db:5432", "https://example.com/health"}, BillingDay: 1, QuotaBytes: "0",
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

func TestReportProbeResultValidation(t *testing.T) {
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	latency, status := 12.5, 200
	valid := v1.ProbeResult{
		Target: "https://example.com/health", OK: true, LatencyMS: &latency, StatusCode: &status,
		CheckedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	report.ProbeResults = []v1.ProbeResult{valid}
	if err := report.Validate(); err != nil {
		t.Fatalf("valid probe result rejected: %v", err)
	}
	failure := valid
	failure.OK = false
	failure.LatencyMS = nil
	failure.Error = "timeout"
	failure.StatusCode = nil
	if err := failure.Validate(); err != nil {
		t.Fatalf("valid failed probe result rejected: %v", err)
	}
	httpFailure := failure
	httpFailure.Error = "http_status"
	httpFailure.StatusCode = &status
	if err := httpFailure.Validate(); err != nil {
		t.Fatalf("valid HTTP status failure rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*v1.ProbeResult)
	}{
		{"invalid_target", func(result *v1.ProbeResult) { result.Target = "udp://host:7" }},
		{"ok_with_error", func(result *v1.ProbeResult) { result.Error = "error" }},
		{"ok_without_latency", func(result *v1.ProbeResult) { result.LatencyMS = nil }},
		{"failure_with_latency", func(result *v1.ProbeResult) { result.OK = false; result.Error = "timeout" }},
		{"invalid_error", func(result *v1.ProbeResult) { result.OK = false; result.LatencyMS = nil; result.Error = "raw error" }},
		{"http_status_without_code", func(result *v1.ProbeResult) {
			result.OK = false
			result.LatencyMS = nil
			result.Error = "http_status"
			result.StatusCode = nil
		}},
		{"status_code_out_of_range", func(result *v1.ProbeResult) { code := 600; result.StatusCode = &code }},
		{"status_code_on_tcp", func(result *v1.ProbeResult) { result.Target = "tcp://host:80" }},
		{"zero_checked_at", func(result *v1.ProbeResult) { result.CheckedAt = time.Time{} }},
		{"negative_latency", func(result *v1.ProbeResult) { value := -1.0; result.LatencyMS = &value }},
		{"infinite_latency", func(result *v1.ProbeResult) { value := math.Inf(1); result.LatencyMS = &value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.mutate(&result)
			report.ProbeResults = []v1.ProbeResult{result}
			if err := report.Validate(); err == nil {
				t.Fatal("invalid probe result accepted")
			}
		})
	}

	report.ProbeResults = []v1.ProbeResult{valid, valid}
	if err := report.Validate(); err == nil {
		t.Fatal("duplicate probe result target accepted")
	}
	report.ProbeResults = make([]v1.ProbeResult, 6)
	if err := report.Validate(); err == nil {
		t.Fatal("too many probe results accepted")
	}
}

func TestMetricStatusesAndMonotonicFreshness(t *testing.T) {
	for _, status := range []string{"ok", "unsupported", "permission_denied", "disabled", "error", "stale"} {
		body := strings.Replace(reportJSON, `"cpu_percent":"stale"`, `"cpu_percent":"`+status+`"`, 1)
		if status == "ok" {
			body = strings.Replace(body, `"cpu_percent":null`, `"cpu_percent":0`, 1)
		}
		var report v1.Report
		if err := json.Unmarshal([]byte(body), &report); err != nil {
			t.Fatal(err)
		}
		if err := report.Validate(); err != nil {
			t.Errorf("status %s rejected: %v", status, err)
		}
	}
	for _, tt := range []struct {
		interval, age int64
		fresh         bool
	}{
		{10000, 45000, true}, {10000, 45001, false}, {60000, 180000, true}, {60000, 180001, false},
	} {
		source := v1.SourceMetadata{SampleID: "one-real-sample", SourceIntervalMS: tt.interval, AgeMS: tt.age}
		if got := source.Fresh(); got != tt.fresh {
			t.Errorf("interval=%d age=%d fresh=%v, want %v", tt.interval, tt.age, got, tt.fresh)
		}
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
			if err := (v1.DesiredConfig{DiskPaths: paths, Interfaces: interfaces}).ValidateSelection(); err == nil {
				t.Fatal("selection can expand beyond the report budget")
			}
		})
	}
	paths, interfaces := make([]string, 8), make([]string, 8)
	for i := range paths {
		paths[i] = "/" + strings.Repeat("a", 1022) + string(rune('a'+i))
		interfaces[i] = strings.Repeat("a", 254) + string(rune('a'+i))
	}
	if err := (v1.DesiredConfig{DiskPaths: paths, Interfaces: interfaces}).ValidateSelection(); err != nil {
		t.Fatalf("ordinary maximum-length selections rejected: %v", err)
	}
	probes := make([]string, 5)
	for i := range probes {
		suffix := "?id=" + strconv.Itoa(i)
		probes[i] = "https://example.com/" + strings.Repeat("<", 1024-len("https://example.com/")-len(suffix)) + suffix
	}
	if err := (v1.DesiredConfig{DiskPaths: paths, Interfaces: interfaces, ProbeTargets: probes}).ValidateSelection(); err == nil {
		t.Fatal("probe results can expand beyond the report budget")
	}
	probeHeavy := v1.DesiredConfig{DiskPaths: paths, ProbeTargets: probes}
	if err := probeHeavy.ValidateSelection(); err == nil {
		t.Fatal("probe target expansion alone can exceed the report budget")
	} else if validation, ok := err.(*v1.ValidationError); !ok || validation.Fields["probe_targets"] == "" {
		t.Fatalf("probe target budget error missing probe_targets field: %v", err)
	}
}
