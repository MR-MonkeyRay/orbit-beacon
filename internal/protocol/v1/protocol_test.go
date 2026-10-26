package v1_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

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
 "capabilities":{"self_update":false},
 "traffic":[{"epoch":"boot-a","period_start":"2026-10-01T00:00:00Z","rx_bytes":"18446744073709551615","tx_bytes":"9007199254740993"}],
 "probe_results":[],"update_status":{"job_id":"job-a","status":"healthy","release_seq":42}
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
	var enrolled v1.EnrollmentResponse
	assertJSONRoundTrip(t, `{"node_id":"server-bound-id","server_time":"2026-10-26T12:00:00Z"}`, &enrolled)
	var response v1.ReportResponse
	assertJSONRoundTrip(t, `{"accepted_seq":9,"server_time":"2026-10-26T12:00:00Z","desired_config":{"revision":4,"report_interval_seconds":30,"disk_interval_seconds":60,"interfaces":["eth0"],"disk_paths":["/"],"probe_targets":[],"billing_day":1,"quota_bytes":"9007199254740993"},"update_job":{"job_id":"job-a","release_seq":42},"traffic_acks":[{"epoch":"boot-a","period_start":"2026-10-01T00:00:00Z","rx_bytes":"18446744073709551615","tx_bytes":"9007199254740993"}]}`, &response)
}

func TestTrafficCountersRejectLossyOrNonCanonicalJSON(t *testing.T) {
	for _, value := range []string{`9007199254740993`, `"-1"`, `"1.5"`, `"1e6"`, `"18446744073709551616"`, `"01"`, `null`} {
		t.Run(value, func(t *testing.T) {
			body := strings.Replace(reportJSON, `"18446744073709551615"`, value, 1)
			var report v1.Report
			if err := json.Unmarshal([]byte(body), &report); err == nil {
				if err := report.Validate(); err == nil {
					t.Fatalf("accepted invalid cumulative counter %s", value)
				}
			}
		})
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
		source := v1.Source{SampleID: "one-real-sample", SourceIntervalMS: tt.interval, AgeMS: tt.age}
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
}
