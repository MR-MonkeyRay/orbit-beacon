package main

import (
	"fmt"
	"strconv"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func makeReport(o options, n node, seq uint64, now time.Time) v1.Report {
	report := v1.Report{ProtocolVersion: v1.ProtocolVersion, RunGeneration: 1, SessionID: n.session, Seq: seq, SampledAt: now, System: &n.system, Runtime: v1.DefaultRuntimeSettings()}
	report.Runtime.ReportIntervalSeconds = int(o.interval / time.Second)
	cpu, load, uptime := float64(20+seq%30), 0.5, float64(seq)*o.interval.Seconds()
	memoryTotal, memoryAvailable, swapTotal, swapUsed := uint64(8<<30), uint64(4<<30), uint64(1<<30), uint64(0)
	report.Metrics = v1.Metrics{CPUPercent: &cpu, Load1: &load, Load5: &load, Load15: &load, MemoryTotalBytes: &memoryTotal, MemoryAvailableBytes: &memoryAvailable, SwapTotalBytes: &swapTotal, SwapUsedBytes: &swapUsed, UptimeSeconds: &uptime, Disks: []v1.DiskMetrics{}, Networks: []v1.NetworkMetrics{}, MetricStatus: make(map[string]string), Sources: make(map[string]v1.SourceMetadata)}
	for _, field := range v1.MetricStatusFields() {
		report.Metrics.MetricStatus[field] = "ok"
	}
	window := o.interval.Milliseconds()
	source := v1.SourceMetadata{SampleID: strconv.FormatUint(seq, 10), SourceIntervalMS: window, MeasurementWindowMS: &window}
	for _, family := range v1.SourceFamilies() {
		report.Metrics.Sources[family] = source
	}
	diskTotal, inodesTotal := uint64(100<<30), uint64(10_000_000)
	for i := range o.disks {
		diskUsed, inodesUsed := uint64(i%10+1)*(10<<30), uint64(i%10+1)*1_000_000
		path := fmt.Sprintf("/load/disk%d", i)
		report.Runtime.DiskPaths = append(report.Runtime.DiskPaths, path)
		report.Metrics.Disks = append(report.Metrics.Disks, v1.DiskMetrics{Path: path, TotalBytes: &diskTotal, UsedBytes: &diskUsed, InodesTotal: &inodesTotal, InodesUsed: &inodesUsed, SampledAt: now})
		key := v1.DiskMetricKey(path)
		report.Metrics.MetricStatus[key] = "ok"
		diskSource := source
		diskSource.SourceIntervalMS = 60000
		diskSource.MeasurementWindowMS = nil
		report.Metrics.Sources[key] = diskSource
	}
	for i := range o.networks {
		name := fmt.Sprintf("eth%d", i)
		rx, tx := float64((i+1)*1024), float64((i+1)*512)
		report.Runtime.Interfaces = append(report.Runtime.Interfaces, name)
		report.Metrics.Networks = append(report.Metrics.Networks, v1.NetworkMetrics{Name: name, RxBytesPerSec: &rx, TxBytesPerSec: &tx})
		key := v1.NetworkMetricKey(name)
		report.Metrics.MetricStatus[key] = "ok"
		report.Metrics.Sources[key] = source
	}
	// 累计量与每小时桶遵循真实协议，模拟数据不读取本机或探测外部服务。
	bytes := seq * uint64(o.interval/time.Second) * uint64(o.networks*(o.networks+1)/2) * 1024
	report.Traffic = []v1.Traffic{{Epoch: n.epoch, PeriodStart: now.Truncate(time.Hour), RxBytes: v1.DecimalUint64(strconv.FormatUint(bytes, 10)), TxBytes: v1.DecimalUint64(strconv.FormatUint(bytes/2, 10))}}
	for i := range o.probes {
		target := fmt.Sprintf("tcp://load-%d.invalid:443", i+1)
		latency := float64(i + 1)
		report.Runtime.ProbeTargets = append(report.Runtime.ProbeTargets, target)
		report.ProbeResults = append(report.ProbeResults, v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, CheckedAt: now})
	}
	return report
}
