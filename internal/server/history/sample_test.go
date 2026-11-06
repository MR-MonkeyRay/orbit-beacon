package history

import (
	"math"
	v1 "orbitbeacon/internal/protocol/v1"
	"testing"
	"time"
)

func ptr[T any](value T) *T { return &value }

func TestSnapshotScalarMathAndOwnership(t *testing.T) {
	m := v1.Metrics{
		CPUPercent: ptr(25.0), Load1: ptr(1.5), Load5: ptr(2.0), Load15: ptr(3.0), UptimeSeconds: ptr(100.0),
		MemoryTotalBytes: ptr(uint64(1000)), MemoryAvailableBytes: ptr(uint64(250)), SwapTotalBytes: ptr(uint64(100)), SwapUsedBytes: ptr(uint64(20)),
		Disks:        []v1.DiskMetrics{{Path: "/", TotalBytes: ptr(uint64(100)), UsedBytes: ptr(uint64(30))}, {Path: "/data", TotalBytes: ptr(uint64(300)), UsedBytes: ptr(uint64(170))}},
		Networks:     []v1.NetworkMetrics{{Name: "eth0", RxBytesPerSec: ptr(10.0), TxBytesPerSec: ptr(30.0)}, {Name: "eth1", RxBytesPerSec: ptr(40.0), TxBytesPerSec: ptr(20.0)}},
		MetricStatus: map[string]string{}, Sources: map[string]v1.SourceMetadata{},
	}
	for _, key := range []string{"cpu_percent", "load1", "load5", "load15", "uptime_seconds", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes", "disk:/", "disk:/data", "network:eth0", "network:eth1"} {
		m.MetricStatus[key] = "ok"
	}
	for _, key := range []string{"cpu", "load", "memory", "swap", "uptime", "disk:/", "disk:/data", "network:eth0", "network:eth1"} {
		m.Sources[key] = v1.SourceMetadata{SampleID: "one", SourceIntervalMS: 10000}
	}
	now := time.Now().UTC()
	sample := Snapshot("node", now, m)
	for metric, want := range map[string]float64{"cpu_percent": 25, "memory_used_percent": 75, "swap_used_percent": 20, "load1": 1.5, "load5": 2, "load15": 3, "disk_used_percent": 50, "net_rx_bytes_per_second": 50, "net_tx_bytes_per_second": 50} {
		if got, exists := sample.Values[metric]; !exists || got != want {
			t.Errorf("%s=%v exists=%v want=%v", metric, got, exists, want)
		}
	}
	if len(sample.Values) != 11 {
		t.Fatalf("unexpected snapshot metrics: %v", sample.Values)
	}
	for _, metric := range []string{"uptime_seconds", "memory_total_bytes", "memory_used_bytes", "swap_total_bytes", "swap_used_bytes", "disk_total_bytes", "disk_used_bytes"} {
		if _, exists := sample.Values[metric]; exists {
			t.Errorf("unused history metric %s recorded", metric)
		}
	}
	*m.CPUPercent = 99
	*m.Networks[0].RxBytesPerSec = 99
	if sample.Values["cpu_percent"] != 25 || sample.Values["net_rx_bytes_per_second"] != 50 || sample.NodeID != "node" || !sample.ReceivedAt.Equal(now) {
		t.Fatal("snapshot retained caller pointers")
	}
}

func TestNetworkMissingFailureAndZero(t *testing.T) {
	for _, test := range []struct {
		name, status   string
		rx, tx         *float64
		fresh          bool
		wantRX, wantTX bool
	}{
		{"missing", "ok", nil, nil, true, false, false}, {"failed", "error", ptr(12.0), ptr(5.0), true, false, false},
		{"zero", "ok", ptr(0.0), ptr(0.0), true, true, true}, {"one direction", "ok", ptr(0.0), nil, true, true, false},
		{"stale", "ok", ptr(0.0), ptr(0.0), false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := v1.SourceMetadata{SampleID: "network", SourceIntervalMS: 10000}
			if !test.fresh {
				source.AgeMS = 60000
			}
			m := v1.Metrics{Networks: []v1.NetworkMetrics{{Name: "eth0", RxBytesPerSec: test.rx, TxBytesPerSec: test.tx}}, MetricStatus: map[string]string{"network:eth0": test.status}, Sources: map[string]v1.SourceMetadata{"network:eth0": source}}
			sample := Snapshot("node", time.Now(), m)
			_, hasRX := sample.Values["net_rx_bytes_per_second"]
			_, hasTX := sample.Values["net_tx_bytes_per_second"]
			if hasRX != test.wantRX || hasTX != test.wantTX {
				t.Fatalf("values=%v", sample.Values)
			}
		})
	}
}

func TestFailedAndZeroCapacityDoNotBecomePercentZero(t *testing.T) {
	m := v1.Metrics{CPUPercent: ptr(80.0), SwapTotalBytes: ptr(uint64(0)), SwapUsedBytes: ptr(uint64(0)), MetricStatus: map[string]string{"cpu_percent": "error", "swap_total_bytes": "ok", "swap_used_bytes": "ok"}, Sources: map[string]v1.SourceMetadata{"cpu": {SampleID: "cpu", SourceIntervalMS: 10000}, "swap": {SampleID: "swap", SourceIntervalMS: 10000}}}
	sample := Snapshot("node", time.Now(), m)
	if _, ok := sample.Values["cpu_percent"]; ok {
		t.Fatal("failed CPU recorded")
	}
	if _, ok := sample.Values["swap_used_percent"]; ok {
		t.Fatal("zero capacity converted to percent")
	}
}

func TestSnapshotRejectsUnboundedMetrics(t *testing.T) {
	for _, value := range []float64{-1, 1e308, math.Inf(1), math.Inf(-1), math.NaN()} {
		m := v1.Metrics{
			CPUPercent: ptr(value), Load1: ptr(value), Load5: ptr(value), Load15: ptr(value), UptimeSeconds: ptr(value),
			Networks:     []v1.NetworkMetrics{{Name: "eth0", RxBytesPerSec: ptr(value), TxBytesPerSec: ptr(value)}},
			MetricStatus: map[string]string{}, Sources: map[string]v1.SourceMetadata{},
		}
		for _, key := range []string{"cpu_percent", "load1", "load5", "load15", "uptime_seconds", "network:eth0"} {
			m.MetricStatus[key] = "ok"
		}
		for _, key := range []string{"cpu", "load", "uptime", "network:eth0"} {
			m.Sources[key] = v1.SourceMetadata{SampleID: "one", SourceIntervalMS: 10000}
		}
		if sample := Snapshot("node", time.Now(), m); len(sample.Values) != 0 {
			t.Fatalf("accepted %v: %v", value, sample.Values)
		}
	}
	for _, test := range []struct {
		metric string
		limit  float64
	}{{"cpu_percent", 100}, {"memory_used_percent", 100}, {"swap_used_percent", 100}, {"disk_used_percent", 100},
		{"inode_used_percent", 100}, {"load1", 1e6}, {"load5", 1e6}, {"load15", 1e6}, {"net_rx_bytes_per_second", 1e15}, {"net_tx_bytes_per_second", 1e15}} {
		for _, value := range []float64{0, test.limit} {
			if !validValue(test.metric, value) {
				t.Errorf("rejected %s=%v", test.metric, value)
			}
		}
		if validValue(test.metric, test.limit*1.01) {
			t.Errorf("accepted %s above %v", test.metric, test.limit)
		}
	}
}

func TestSnapshotNetworkRejectsInvalidInterfacesBeforeSumming(t *testing.T) {
	for _, value := range []float64{-10, 1e308, math.Inf(1), math.NaN()} {
		m := v1.Metrics{
			Networks: []v1.NetworkMetrics{
				{Name: "bad", RxBytesPerSec: ptr(value), TxBytesPerSec: ptr(value)},
				{Name: "good", RxBytesPerSec: ptr(10.0), TxBytesPerSec: ptr(20.0)},
			},
			MetricStatus: map[string]string{"network:bad": "ok", "network:good": "ok"},
			Sources: map[string]v1.SourceMetadata{
				"network:bad": {SampleID: "one", SourceIntervalMS: 10000}, "network:good": {SampleID: "one", SourceIntervalMS: 10000},
			},
		}
		s := Snapshot("node", time.Now(), m)
		if s.Values["net_rx_bytes_per_second"] != 10 || s.Values["net_tx_bytes_per_second"] != 20 {
			t.Fatalf("invalid interface poisoned rates: %v", s.Values)
		}
	}
}

func TestSampleSeriesValueBounds(t *testing.T) {
	for _, tc := range []struct {
		metric string
		limit  float64
	}{
		{"disk_used_percent", 100},
		{"inode_used_percent", 100},
		{"cpu_percent", 100},
		{"memory_used_percent", 100},
		{"swap_used_percent", 100},
		{"load1", 1e6},
		{"load5", 1e6},
		{"load15", 1e6},
		{"net_rx_bytes_per_second", 1e15},
		{"net_tx_bytes_per_second", 1e15},
		{"uptime_seconds", 1e12},
		{"disk_used_bytes", float64(math.MaxUint64)},
	} {
		for _, suffix := range []string{"", ":/x", ":/x:y_percent"} {
			t.Run(tc.metric+suffix, func(t *testing.T) {
				metric := tc.metric + suffix
				for _, value := range []float64{0, tc.limit, tc.limit * 1.01, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
					want := value >= 0 && value <= tc.limit
					if got := ValidValue(metric, value); got != want {
						t.Errorf("ValidValue(%q, %v)=%v, want %v", metric, value, got, want)
					}
					s := &Service{pending: make(map[bucketKey]map[string]aggregate)}
					s.add(Sample{NodeID: "node", ReceivedAt: time.Unix(60, 0), Values: map[string]float64{metric: value}})
					got, accepted := s.pending[bucketKey{"node", 60}][metric]
					if accepted != want || (want && (got.total != value || got.peak != value || got.samples != 1)) {
						t.Errorf("Sample %s=%v: accepted=%v aggregate=%+v, want accepted=%v", metric, value, accepted, got, want)
					}
				}
			})
		}
	}
}

func TestSnapshotDiskAndInodeSeriesFreshnessAndAggregation(t *testing.T) {
	for _, tc := range []struct {
		status   string
		age      int64
		included bool
	}{
		{"ok", 0, true}, {"error", 0, false}, {"stale", 0, false}, {"ok", 180001, false},
	} {
		t.Run(tc.status+(time.Duration(tc.age)*time.Millisecond).String(), func(t *testing.T) {
			m := v1.Metrics{
				Disks: []v1.DiskMetrics{
					{Path: "/a:one", TotalBytes: ptr(uint64(100)), UsedBytes: ptr(uint64(25)), InodesTotal: ptr(uint64(40)), InodesUsed: ptr(uint64(30))},
					{Path: "/b", TotalBytes: ptr(uint64(300)), UsedBytes: ptr(uint64(175)), InodesTotal: ptr(uint64(60)), InodesUsed: ptr(uint64(20))},
					{Path: "/unsupported", TotalBytes: ptr(uint64(100)), UsedBytes: ptr(uint64(50))},
				},
				MetricStatus: map[string]string{"disk:/a:one": tc.status, "disk:/b": "ok", "disk:/unsupported": "ok"},
				Sources:      map[string]v1.SourceMetadata{"disk:/a:one": {SampleID: "a", AgeMS: tc.age, SourceIntervalMS: 60000}, "disk:/b": {SampleID: "b", SourceIntervalMS: 60000}, "disk:/unsupported": {SampleID: "c", SourceIntervalMS: 60000}},
			}
			sample := Snapshot("node", time.Now(), m)
			expected := map[string]float64{"disk_used_percent:/b": 100 * 175.0 / 300, "inode_used_percent:/b": 100 * 20.0 / 60, "disk_used_percent:/unsupported": 50}
			if tc.included {
				expected["disk_used_percent:/a:one"], expected["inode_used_percent:/a:one"] = 25, 75
				expected["disk_used_percent"], expected["inode_used_percent"] = 50, 50
			} else {
				expected["disk_used_percent"], expected["inode_used_percent"] = 100*225.0/400, 100*20.0/60
			}
			if len(sample.Values) != len(expected) {
				t.Fatalf("values=%v want=%v", sample.Values, expected)
			}
			for metric, want := range expected {
				if got, ok := sample.Values[metric]; !ok || math.Abs(got-want) > 1e-10 {
					t.Errorf("%s=%v present=%v want=%v", metric, got, ok, want)
				}
			}
		})
	}
}
