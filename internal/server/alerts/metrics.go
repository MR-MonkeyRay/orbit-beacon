package alerts

import (
	"math"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

// Metric freshness includes time elapsed since reception as well as the source
// age at reception. Agent wall clocks do not establish report liveness.
func metricValue(latest ingest.LatestReport, metric string, now time.Time) *float64 {
	if latest.ReceivedAt.IsZero() || latest.ReceivedAt.After(now) || now.Sub(latest.ReceivedAt) > nodes.OnlineWindow {
		return nil
	}
	m := latest.Metrics
	elapsed := now.Sub(latest.ReceivedAt).Milliseconds()
	ok := func(key, source string) bool {
		s := m.Sources[source]
		if m.MetricStatus[key] != "ok" || !s.Fresh() {
			return false
		}
		return s.FreshAfter(elapsed)
	}
	valid := func(v float64) *float64 {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || (metric != "load1" && v > 100) {
			return nil
		}
		return &v
	}
	switch metric {
	case "probes_down":
		value := 0.0
		for _, probe := range latest.ProbeResults {
			if !probe.OK {
				value++
			}
		}
		return valid(value)
	case "cpu_percent":
		if m.CPUPercent != nil && ok("cpu_percent", "cpu") {
			return valid(*m.CPUPercent)
		}
	case "load1":
		if m.Load1 != nil && ok("load1", "load") {
			return valid(*m.Load1)
		}
	case "memory_used_percent":
		if m.MemoryTotalBytes != nil && m.MemoryAvailableBytes != nil && *m.MemoryTotalBytes > 0 && *m.MemoryAvailableBytes <= *m.MemoryTotalBytes && ok("memory_total_bytes", "memory") && ok("memory_available_bytes", "memory") {
			return valid(100 * float64(*m.MemoryTotalBytes-*m.MemoryAvailableBytes) / float64(*m.MemoryTotalBytes))
		}
	case "swap_used_percent":
		if m.SwapTotalBytes != nil && m.SwapUsedBytes != nil && *m.SwapTotalBytes > 0 && *m.SwapUsedBytes <= *m.SwapTotalBytes && ok("swap_total_bytes", "swap") && ok("swap_used_bytes", "swap") {
			return valid(100 * float64(*m.SwapUsedBytes) / float64(*m.SwapTotalBytes))
		}
	case "disk_used_percent":
		var value *float64
		for _, d := range m.Disks {
			key := v1.DiskMetricKey(d.Path)
			if d.TotalBytes != nil && d.UsedBytes != nil && *d.TotalBytes > 0 && *d.UsedBytes <= *d.TotalBytes && ok(key, key) {
				v := 100 * float64(*d.UsedBytes) / float64(*d.TotalBytes)
				if value == nil || v > *value {
					value = valid(v)
				}
			}
		}
		return value
	}
	return nil
}
func condition(r Rule, value float64, firing bool) bool {
	threshold := *r.Threshold
	margin := 0.0
	if firing {
		margin = math.Abs(threshold) * 0.02
		if r.Metric != "load1" && r.Metric != "probes_down" {
			margin = max(margin, 1)
		}
	}
	if *r.Comparator == ">" {
		if firing {
			return value > max(0, threshold-margin)
		}
		return value > threshold
	}
	if firing {
		boundary := threshold + margin
		if r.Metric == "probes_down" {
			boundary = min(5, boundary)
		} else if r.Metric != "load1" {
			boundary = min(100, boundary)
		}
		return value < boundary
	}
	return value < threshold
}
