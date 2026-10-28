// Package history stores best-effort telemetry independently of the main writer.
package history

import (
	"math"
	"strings"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

// Sample owns its scalar values; it retains no pointers, slices, credentials or
// process identity from the accepted report. Missing/failed metrics are omitted.
type Sample struct {
	NodeID     string
	ReceivedAt time.Time
	Values     map[string]float64
}

// ValidValue exposes admission bounds for persisted monitoring validation.
func ValidValue(metric string, value float64) bool { return validValue(metric, value) }

// Bounded nonnegative values keep sums finite even at an int64 sample count.
// Apply the same limits at admission and aggregation for direct Sample callers.
func validValue(metric string, value float64) bool {
	limit := float64(math.MaxUint64) // byte counters/capacities
	switch {
	case strings.HasSuffix(metric, "_percent"):
		limit = 100
	case metric == "load1" || metric == "load5" || metric == "load15":
		limit = 1e6
	case strings.HasSuffix(metric, "_bytes_per_second"):
		limit = 1e15
	case metric == "uptime_seconds":
		limit = 1e12
	}
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= limit
}

// Snapshot copies only successful, fresh scalar measurements. Network values
// are already rates in protocol v1; a sample sums successful interfaces.
func Snapshot(nodeID string, receivedAt time.Time, m v1.Metrics) Sample {
	s := Sample{NodeID: nodeID, ReceivedAt: receivedAt, Values: make(map[string]float64)}
	ok := func(key, source string) bool { return m.MetricStatus[key] == "ok" && m.Sources[source].Fresh() }
	put := func(key string, value float64) {
		if validValue(key, value) {
			s.Values[key] = value
		}
	}
	for key, value := range map[string]*float64{"cpu_percent": m.CPUPercent, "load1": m.Load1, "load5": m.Load5, "load15": m.Load15, "uptime_seconds": m.UptimeSeconds} {
		source := key
		switch key {
		case "cpu_percent":
			source = "cpu"
		case "load1", "load5", "load15":
			source = "load"
		case "uptime_seconds":
			source = "uptime"
		}
		if value != nil && ok(key, source) {
			put(key, *value)
		}
	}
	if m.MemoryTotalBytes != nil && m.MemoryAvailableBytes != nil && ok("memory_total_bytes", "memory") && ok("memory_available_bytes", "memory") && *m.MemoryAvailableBytes <= *m.MemoryTotalBytes {
		total, used := *m.MemoryTotalBytes, *m.MemoryTotalBytes-*m.MemoryAvailableBytes
		put("memory_total_bytes", float64(total))
		put("memory_used_bytes", float64(used))
		if total > 0 {
			put("memory_used_percent", 100*float64(used)/float64(total))
		}
	}
	if m.SwapTotalBytes != nil && m.SwapUsedBytes != nil && ok("swap_total_bytes", "swap") && ok("swap_used_bytes", "swap") {
		total, used := *m.SwapTotalBytes, *m.SwapUsedBytes
		put("swap_total_bytes", float64(total))
		put("swap_used_bytes", float64(used))
		if total > 0 && used <= total {
			put("swap_used_percent", 100*float64(used)/float64(total))
		}
	}
	var diskTotal, diskUsed float64
	for _, d := range m.Disks {
		key := v1.DiskMetricKey(d.Path)
		if d.TotalBytes != nil && d.UsedBytes != nil && ok(key, key) {
			diskTotal += float64(*d.TotalBytes)
			diskUsed += float64(*d.UsedBytes)
		}
	}
	if diskTotal > 0 {
		put("disk_total_bytes", diskTotal)
		put("disk_used_bytes", diskUsed)
		put("disk_used_percent", 100*diskUsed/diskTotal)
	}
	var rx, tx float64
	var hasRX, hasTX bool
	for _, n := range m.Networks {
		key := v1.NetworkMetricKey(n.Name)
		if !ok(key, key) {
			continue
		}
		if n.RxBytesPerSec != nil && validValue("net_rx_bytes_per_second", *n.RxBytesPerSec) {
			rx += *n.RxBytesPerSec
			hasRX = true
		}
		if n.TxBytesPerSec != nil && validValue("net_tx_bytes_per_second", *n.TxBytesPerSec) {
			tx += *n.TxBytesPerSec
			hasTX = true
		}
	}
	if hasRX {
		put("net_rx_bytes_per_second", rx)
	}
	if hasTX {
		put("net_tx_bytes_per_second", tx)
	}
	return s
}
