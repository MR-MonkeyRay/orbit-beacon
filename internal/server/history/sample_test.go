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
	for metric, want := range map[string]float64{"cpu_percent": 25, "memory_used_bytes": 750, "memory_total_bytes": 1000, "memory_used_percent": 75, "swap_used_bytes": 20, "swap_total_bytes": 100, "swap_used_percent": 20, "load1": 1.5, "load5": 2, "load15": 3, "uptime_seconds": 100, "disk_used_bytes": 200, "disk_total_bytes": 400, "disk_used_percent": 50, "net_rx_bytes_per_second": 50, "net_tx_bytes_per_second": 50} {
		if got, exists := sample.Values[metric]; !exists || got != want {
			t.Errorf("%s=%v exists=%v want=%v", metric, got, exists, want)
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
	}{{"cpu_percent", 100}, {"memory_used_percent", 100}, {"swap_used_percent", 100}, {"disk_used_percent", 100}, {"load1", 1e6}, {"load5", 1e6}, {"load15", 1e6}, {"net_rx_bytes_per_second", 1e15}, {"net_tx_bytes_per_second", 1e15}} {
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
