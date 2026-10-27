package ingest

import (
	"math"
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
	if got.CPUPercent == nil || *got.CPUPercent != 0 || got.MemoryPercent == nil || *got.MemoryPercent != 100 || got.Load1 != nil || got.UptimeSeconds == nil || *got.UptimeSeconds != 3600 {
		t.Fatalf("projection: %+v", got)
	}
	got = PublicNode{}
	snapshot.project(&got, now.Add(35001*time.Millisecond))
	if got.CPUPercent == nil || got.MemoryPercent != nil {
		t.Fatalf("independent expiration: %+v", got)
	}
	got = PublicNode{}
	snapshot.project(&got, now.Add(45000*time.Millisecond))
	if got.CPUPercent == nil {
		t.Fatal("exact freshness boundary lost")
	}
	got = PublicNode{}
	snapshot.project(&got, now.Add(45001*time.Millisecond))
	if got.CPUPercent != nil || got.UptimeSeconds == nil {
		t.Fatalf("overflow-safe expiration: %+v", got)
	}
	got = PublicNode{}
	snapshot.project(&got, now.Add(-time.Nanosecond))
	if got.CPUPercent != nil || got.UptimeSeconds != nil {
		t.Fatal("future snapshot exposed")
	}
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
