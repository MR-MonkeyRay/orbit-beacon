package ingest

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestPublicSnapshotFreshnessAndIndependentAvailability(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	cpu, load, uptime := 0.0, 0.5, 3600.0
	total, available := uint64(math.MaxUint64), uint64(0)
	metrics := v1.Metrics{CPUPercent: &cpu, Load1: &load, UptimeSeconds: &uptime, MemoryTotalBytes: &total, MemoryAvailableBytes: &available,
		MetricStatus: map[string]string{"cpu_percent": "ok", "load1": "stale", "uptime_seconds": "ok", "memory_total_bytes": "ok", "memory_available_bytes": "ok"},
		Sources:      map[string]v1.SourceMetadata{"cpu": {AgeMS: 0, SourceIntervalMS: 10000}, "memory": {AgeMS: 10000, SourceIntervalMS: 10000}, "load": {SourceIntervalMS: 10000}, "uptime": {SourceIntervalMS: math.MaxInt64}}}
	snapshot := snapshotPublic(metrics, now)
	cpu = 99
	total = 0 // Snapshot must own values, not the caller's report pointers.
	var got PublicNode
	snapshot.project(&got, now.Add(35*time.Second))
	if got.CPUPercent == nil || *got.CPUPercent != 0 || got.MemoryPercent == nil || *got.MemoryPercent != 100 || got.Load1 == nil || *got.Load1 != .5 || got.UptimeSeconds == nil || *got.UptimeSeconds != 3600 {
		t.Fatalf("projection: %+v", got)
	}
	assertPublicStatuses(t, got, "ok", "ok", "stale", "ok")
	got = PublicNode{}
	snapshot.project(&got, now.Add(35001*time.Millisecond))
	if got.CPUPercent == nil || got.MemoryPercent == nil || *got.MemoryPercent != 100 {
		t.Fatalf("independent expiration: %+v", got)
	}
	assertPublicStatuses(t, got, "ok", "stale", "stale", "ok")
	got = PublicNode{}
	snapshot.project(&got, now.Add(45000*time.Millisecond))
	if got.CPUPercent == nil {
		t.Fatal("exact freshness boundary lost")
	}
	assertPublicStatuses(t, got, "ok", "stale", "stale", "ok")
	got = PublicNode{}
	snapshot.project(&got, now.Add(45001*time.Millisecond))
	if got.CPUPercent == nil || *got.CPUPercent != 0 || got.UptimeSeconds == nil {
		t.Fatalf("overflow-safe expiration: %+v", got)
	}
	assertPublicStatuses(t, got, "stale", "stale", "stale", "ok")
	got = PublicNode{}
	snapshot.project(&got, now.Add(-time.Nanosecond))
	if got.CPUPercent != nil || got.UptimeSeconds != nil {
		t.Fatal("future snapshot exposed")
	}
	assertPublicStatuses(t, got, "unavailable", "unavailable", "unavailable", "unavailable")
	metrics.MemoryTotalBytes = &total
	snapshot = snapshotPublic(metrics, now)
	got = PublicNode{}
	snapshot.project(&got, now)
	if got.MemoryPercent != nil {
		t.Fatal("zero total memory exposed")
	}
	for _, test := range []struct {
		name    string
		metric  publicMetric
		elapsed int64
		want    bool
	}{
		{"already stale", publicMetric{valid: true, ageMS: 45001, intervalMS: 10000}, 0, false},
		{"large age", publicMetric{valid: true, ageMS: math.MaxInt64, intervalMS: math.MaxInt64}, math.MaxInt64, true},
		{"bounded large age", publicMetric{valid: true, ageMS: math.MaxInt64, intervalMS: 10000}, 0, false},
		{"failed status", publicMetric{valid: false, intervalMS: 10000}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.metric.fresh(test.elapsed); got != test.want {
				t.Fatalf("fresh=%v want=%v", got, test.want)
			}
		})
	}
}

func assertPublicStatuses(t *testing.T, node PublicNode, cpu, memory, load, uptime string) {
	t.Helper()
	body, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Status map[string]string `json:"metric_status"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"cpu_percent": cpu, "memory_percent": memory, "load1": load, "uptime_seconds": uptime}
	if !reflect.DeepEqual(wire.Status, want) {
		t.Fatalf("metric_status=%v want=%v body=%s", wire.Status, want, body)
	}
}

func TestPublicSnapshotRejectsUnavailableOrIllegalNumbers(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		value  *float64
		status string
	}{
		{"missing", nil, "stale"},
		{"error with value", new(25.0), "error"},
		{"unsupported with value", new(25.0), "unsupported"},
		{"negative", new(-1.0), "ok"},
		{"too large", new(101.0), "ok"},
		{"nonfinite", new(math.Inf(1)), "stale"},
		{"nan", new(math.NaN()), "ok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := v1.Metrics{CPUPercent: test.value, MetricStatus: map[string]string{"cpu_percent": test.status}, Sources: map[string]v1.SourceMetadata{"cpu": {SourceIntervalMS: 10000}}}
			var node PublicNode
			snapshotPublic(m, now).project(&node, now)
			if node.CPUPercent != nil {
				t.Fatalf("invalid CPU exposed: %+v", node)
			}
			assertPublicStatuses(t, node, "unavailable", "unavailable", "unavailable", "unavailable")
		})
	}
}

func TestPublicMemoryNeedsBothCurrentValuesAndStatuses(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name             string
		total, available *uint64
		totalStatus      string
		availableStatus  string
		want             string
	}{
		{"fresh", new(uint64(100)), new(uint64(25)), "ok", "ok", "ok"},
		{"stale total", new(uint64(100)), new(uint64(25)), "stale", "ok", "stale"},
		{"stale available", new(uint64(100)), new(uint64(25)), "ok", "stale", "stale"},
		{"failed total", new(uint64(100)), new(uint64(25)), "error", "ok", "unavailable"},
		{"unsupported available", new(uint64(100)), new(uint64(25)), "ok", "unsupported", "unavailable"},
		{"missing available", new(uint64(100)), nil, "ok", "stale", "unavailable"},
		{"zero total", new(uint64(0)), new(uint64(0)), "ok", "ok", "unavailable"},
		{"invalid ratio", new(uint64(100)), new(uint64(101)), "ok", "ok", "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := v1.Metrics{MemoryTotalBytes: test.total, MemoryAvailableBytes: test.available,
				MetricStatus: map[string]string{"memory_total_bytes": test.totalStatus, "memory_available_bytes": test.availableStatus},
				Sources:      map[string]v1.SourceMetadata{"memory": {SourceIntervalMS: 10000}}}
			var node PublicNode
			snapshotPublic(m, now).project(&node, now)
			if test.want == "unavailable" {
				if node.MemoryPercent != nil {
					t.Fatalf("unavailable memory=%+v", node)
				}
			} else if node.MemoryPercent == nil || *node.MemoryPercent != 75 {
				t.Fatalf("available memory=%+v", node)
			}
			assertPublicStatuses(t, node, "unavailable", test.want, "unavailable", "unavailable")
		})
	}
}
