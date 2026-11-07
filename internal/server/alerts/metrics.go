package alerts

import (
	"math"
	"slices"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

// 指标时效既考虑接收后经过的时间，也考虑接收时源数据已存在多久。
// 上报是否仍有效不能由 Agent 的系统时间决定。
func metricValue(latest ingest.LatestReport, r Rule, now time.Time) *float64 {
	metric := r.Metric
	if !latestFresh(latest, now) {
		return nil
	}
	if metricDisabled(r, latest.Runtime) {
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
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || (metric != "load1" && metric != "probe_latency_ms" && metric != "probe_tls_expiry_days" && v > 100) {
			return nil
		}
		return &v
	}
	switch metric {
	case "probe_failed", "probe_latency_ms", "probe_tls_expiry_days":
		if r.Target == nil {
			return nil
		}
		for _, probe := range latest.ProbeResults {
			if probe.Target != *r.Target {
				continue
			}
			switch metric {
			case "probe_failed":
				if probe.OK {
					return valid(0)
				}
				return valid(1)
			case "probe_latency_ms":
				if probe.OK && probe.LatencyMS != nil {
					return valid(*probe.LatencyMS)
				}
			case "probe_tls_expiry_days":
				if probe.TLSExpiresAt != nil {
					return valid(max(0, probe.TLSExpiresAt.Sub(now).Hours()/24))
				}
			}
			return nil
		}
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

func latestFresh(latest ingest.LatestReport, now time.Time) bool {
	return !latest.ReceivedAt.IsZero() && !latest.ReceivedAt.After(now) && now.Sub(latest.ReceivedAt) <= nodes.OnlineWindow
}

func targetMetric(metric string) bool {
	return metric == "probe_failed" || metric == "probe_latency_ms" || metric == "probe_tls_expiry_days"
}

// metricDisabled 判断指标是否被 Agent 本地关闭或目标已移除。明确关闭不是数据缺失。
func metricDisabled(r Rule, runtime v1.RuntimeSettings) bool {
	if targetMetric(r.Metric) {
		return runtime.FeatureDisabled(v1.FeatureProbes) || r.Target == nil || !slices.Contains(runtime.ProbeTargets, *r.Target)
	}
	switch r.Metric {
	case "traffic_quota_percent":
		return runtime.FeatureDisabled(v1.FeatureTraffic)
	case "disk_used_percent":
		return runtime.FeatureDisabled(v1.FeatureDisk)
	case "probes_down":
		return runtime.FeatureDisabled(v1.FeatureProbes)
	}
	return false
}

func condition(r Rule, value float64, firing bool) bool {
	threshold := *r.Threshold
	margin := 0.0
	if firing {
		margin = math.Abs(threshold) * 0.02
		if r.Metric != "load1" && r.Metric != "probes_down" && r.Metric != "probe_latency_ms" {
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
		} else if r.Metric != "load1" && r.Metric != "traffic_quota_percent" && r.Metric != "node_expiry_days" && r.Metric != "probe_latency_ms" && r.Metric != "probe_tls_expiry_days" {
			boundary = min(100, boundary)
		}
		return value < boundary
	}
	return value < threshold
}
