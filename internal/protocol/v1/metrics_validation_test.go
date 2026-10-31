package v1_test

import (
	"encoding/json"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
)

func completeMetrics(t *testing.T) v1.Metrics {
	t.Helper()
	var report v1.Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	return report.Metrics
}

// 基础指标仅支持以下类别别名：cpu -> cpu_percent、load -> load1/load5/load15、
// memory -> total/available、swap -> total/used、uptime -> uptime_seconds。
// disk/network 来源使用完整的序列键。
func TestMetricsRequireExactStatusAndDocumentedFamilySources(t *testing.T) {
	families := map[string][]string{
		"cpu":          {"cpu_percent"},
		"load":         {"load1", "load5", "load15"},
		"memory":       {"memory_total_bytes", "memory_available_bytes"},
		"swap":         {"swap_total_bytes", "swap_used_bytes"},
		"uptime":       {"uptime_seconds"},
		"disk:/":       {"disk:/"},
		"network:eth0": {"network:eth0"},
	}
	if err := completeMetrics(t).Validate(); err != nil {
		t.Fatalf("complete documented family sources rejected: %v", err)
	}
	for family, fields := range families {
		t.Run("missing source "+family, func(t *testing.T) {
			m := completeMetrics(t)
			delete(m.Sources, family)
			if err := m.Validate(); err == nil {
				t.Fatalf("accepted missing source for %s", family)
			}
		})
		for _, field := range fields {
			t.Run("missing status "+field, func(t *testing.T) {
				m := completeMetrics(t)
				delete(m.MetricStatus, field)
				if err := m.Validate(); err == nil {
					t.Fatalf("accepted missing status for %s", field)
				}
			})
		}
	}
	for _, key := range []string{"unknown", "cpu_percent", "load1", "memory_total_bytes", "disk:/not-reported", "network:ghost"} {
		t.Run("unknown source "+key, func(t *testing.T) {
			m := completeMetrics(t)
			m.Sources[key] = m.Sources["cpu"]
			if err := m.Validate(); err == nil {
				t.Fatalf("accepted unrecognized source %q", key)
			}
		})
	}
	for _, key := range []string{"unknown", "cpu", "load", "memory", "disk:/not-reported", "network:ghost"} {
		t.Run("unknown status "+key, func(t *testing.T) {
			m := completeMetrics(t)
			m.MetricStatus[key] = "ok"
			if err := m.Validate(); err == nil {
				t.Fatalf("accepted unrecognized status %q", key)
			}
		})
	}
}

func TestUnavailableMetricsStillRequireTheirOwnSources(t *testing.T) {
	for _, family := range []string{"cpu", "load", "memory", "swap", "uptime", "disk:/", "network:eth0"} {
		t.Run(family, func(t *testing.T) {
			m := completeMetrics(t)
			switch family {
			case "cpu":
				m.CPUPercent = nil
				m.MetricStatus["cpu_percent"] = "unsupported"
			case "load":
				m.Load1, m.Load5, m.Load15 = nil, nil, nil
				for _, key := range []string{"load1", "load5", "load15"} {
					m.MetricStatus[key] = "unsupported"
				}
			case "memory":
				m.MemoryTotalBytes, m.MemoryAvailableBytes = nil, nil
				m.MetricStatus["memory_total_bytes"], m.MetricStatus["memory_available_bytes"] = "error", "error"
			case "swap":
				m.SwapTotalBytes, m.SwapUsedBytes = nil, nil
				m.MetricStatus["swap_total_bytes"], m.MetricStatus["swap_used_bytes"] = "disabled", "disabled"
			case "uptime":
				m.UptimeSeconds = nil
				m.MetricStatus["uptime_seconds"] = "error"
			case "disk:/":
				m.Disks[0].TotalBytes, m.Disks[0].UsedBytes = nil, nil
				m.MetricStatus[family] = "permission_denied"
			case "network:eth0":
				m.MetricStatus[family] = "stale"
			}
			if err := m.Validate(); err != nil {
				t.Fatalf("unavailable metric with source rejected: %v", err)
			}
			delete(m.Sources, family)
			if err := m.Validate(); err == nil {
				t.Fatal("unavailable metric accepted without source metadata")
			}
		})
	}
}

func TestPopulatedDiskAndNetworkRequireStatusAndSource(t *testing.T) {
	for _, series := range []string{"disk:/", "network:eth0"} {
		for _, missing := range []string{"status", "source"} {
			t.Run(series+" missing "+missing, func(t *testing.T) {
				m := completeMetrics(t)
				if series == "network:eth0" {
					zero := 0.0
					m.Networks[0].RxBytesPerSec, m.Networks[0].TxBytesPerSec = &zero, &zero
					m.MetricStatus[series] = "ok"
				}
				if missing == "status" {
					delete(m.MetricStatus, series)
				} else {
					delete(m.Sources, series)
				}
				if err := m.Validate(); err == nil {
					t.Fatalf("populated %s accepted without %s", series, missing)
				}
			})
		}
	}
}

func TestPopulatedCPUMetricRequiresExplicitStatus(t *testing.T) {
	m := completeMetrics(t)
	cpu := 12.5
	m.CPUPercent = &cpu
	delete(m.MetricStatus, "cpu_percent")
	if err := m.Validate(); err == nil {
		t.Fatal("populated CPU accepted without explicit metric status")
	}
}

func TestPopulatedCPUMetricRequiresItsFamilySource(t *testing.T) {
	m := completeMetrics(t)
	cpu := 12.5
	m.CPUPercent = &cpu
	m.MetricStatus["cpu_percent"] = "ok"
	delete(m.Sources, "cpu")
	if err := m.Validate(); err == nil {
		t.Fatal("populated CPU accepted without source metadata")
	}
}

func TestDifferentialSourcesRequireMeasurementWindowForEveryStatus(t *testing.T) {
	for _, family := range []string{"cpu", "network:eth0"} {
		for _, status := range []string{"ok", "stale", "unsupported", "permission_denied", "disabled", "error"} {
			t.Run(family+"/"+status, func(t *testing.T) {
				base := completeMetrics(t)
				zero := 0.0
				if family == "cpu" {
					base.CPUPercent = nil
					base.MetricStatus["cpu_percent"] = status
					if status == "ok" {
						base.CPUPercent = &zero
					}
				} else {
					base.Networks[0].RxBytesPerSec, base.Networks[0].TxBytesPerSec = nil, nil
					base.MetricStatus[family] = status
					if status == "ok" {
						base.Networks[0].RxBytesPerSec, base.Networks[0].TxBytesPerSec = &zero, &zero
					}
				}
				for _, window := range []struct {
					name  string
					value *int64
					valid bool
				}{
					{"missing", nil, false},
					{"negative", new(int64(-1)), false},
					{"zero", new(int64(0)), true},
					{"positive", new(int64(1000)), true},
				} {
					t.Run(window.name, func(t *testing.T) {
						source := base.Sources[family]
						source.MeasurementWindowMS = window.value
						base.Sources[family] = source
						err := base.Validate()
						if window.valid && err != nil {
							t.Errorf("nonnegative window rejected: %v", err)
						}
						if !window.valid && err == nil {
							t.Errorf("accepted %s measurement window for %s status %s", window.name, family, status)
						}
					})
				}
			})
		}
	}
}

func TestEveryNetworkSeriesRequiresItsOwnMeasurementWindow(t *testing.T) {
	m := completeMetrics(t)
	m.Networks = append(m.Networks, v1.NetworkMetrics{Name: "eth1"})
	m.MetricStatus["network:eth1"] = "unsupported"
	source := m.Sources["network:eth0"]
	source.SampleID = "eth1-unavailable"
	source.MeasurementWindowMS = nil
	m.Sources["network:eth1"] = source
	if err := m.Validate(); err == nil {
		t.Fatal("accepted a second unavailable network without its own measurement window")
	}
	source.MeasurementWindowMS = new(int64(0))
	m.Sources["network:eth1"] = source
	if err := m.Validate(); err != nil {
		t.Fatalf("zero unavailable-network window rejected: %v", err)
	}
}
