package collect_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/agent/collect"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestMaximumOrdinarySelectionsProduceSendableReport(t *testing.T) {
	config := v1.DefaultRuntimeSettings()
	base := os.TempDir() + string(filepath.Separator)
	for i := range 8 {
		config.DiskPaths = append(config.DiskPaths, base+strings.Repeat("a", 1023-len(base))+string(rune('a'+i)))
		config.Interfaces = append(config.Interfaces, strings.Repeat("a", 254)+string(rune('a'+i)))
	}
	for i := range 5 {
		prefix := "https://probe" + strconv.Itoa(i) + ".example/"
		config.ProbeTargets = append(config.ProbeTargets, prefix+strings.Repeat("a", 1024-len(prefix)))
	}
	collector, err := collect.New(collect.Options{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	metrics, _ := collector.Collect(ctx)
	// 用采集器可输出的最大固定字段覆盖预留，而不依赖测试宿主机的读数。
	maxUint, maxFloat, cpuPercent := uint64(math.MaxUint64), math.MaxFloat64, 100.0
	metrics.CPUPercent = &cpuPercent
	metrics.Load1, metrics.Load5, metrics.Load15, metrics.UptimeSeconds = &maxFloat, &maxFloat, &maxFloat, &maxFloat
	metrics.MemoryTotalBytes, metrics.MemoryAvailableBytes, metrics.SwapTotalBytes, metrics.SwapUsedBytes = &maxUint, &maxUint, &maxUint, &maxUint
	at := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	for i := range metrics.Disks {
		metrics.Disks[i].TotalBytes, metrics.Disks[i].UsedBytes, metrics.Disks[i].SampledAt = &maxUint, &maxUint, at
	}
	for i := range metrics.Networks {
		metrics.Networks[i].RxBytesPerSec, metrics.Networks[i].TxBytesPerSec = &maxFloat, &maxFloat
	}
	for key := range metrics.MetricStatus {
		metrics.MetricStatus[key] = "permission_denied"
	}
	for key, source := range metrics.Sources {
		source.SampleID = strings.Repeat("s", 35)
		source.AgeMS, source.SourceIntervalMS = math.MaxInt64, 3600000
		if source.MeasurementWindowMS != nil {
			window := int64(math.MaxInt64)
			source.MeasurementWindowMS = &window
		}
		metrics.Sources[key] = source
	}
	config.DisabledFeatures = v1.DisabledFeatureOrder() // 最大枚举编码，独立于采集开关覆盖预留。
	report := v1.Report{ProtocolVersion: 1, RunGeneration: math.MaxUint64, SessionID: strings.Repeat("s", 255), Seq: math.MaxUint64, SampledAt: at, Runtime: config, Metrics: metrics, Traffic: []v1.Traffic{}, System: &v1.System{Hostname: strings.Repeat("<", 255), OS: strings.Repeat("&", 64), Arch: strings.Repeat(">", 64), AgentVersion: strings.Repeat("a", 64)}}
	for range 2 {
		report.CommandResults = append(report.CommandResults, v1.CommandResult{ID: "ffffffff-ffff-4fff-bfff-ffffffffffff", Status: "rejected", Code: "disabled_locally"})
	}
	for i := range 24 {
		report.Traffic = append(report.Traffic, v1.Traffic{Epoch: strings.Repeat("f", 64), PeriodStart: time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(i) * time.Hour), RxBytes: "9223372036854775807", TxBytes: "9223372036854775807"})
	}
	for _, target := range config.ProbeTargets {
		latency, code := 59999.999999, 200
		report.ProbeResults = append(report.ProbeResults, v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, StatusCode: &code, CheckedAt: time.Now().UTC()})
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("selected series lost report validity: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	selectionBytes := 0
	for _, values := range [][]string{config.Interfaces, config.DiskPaths} {
		for _, value := range values {
			encoded, _ := json.Marshal(value)
			selectionBytes += 4 * len(encoded)
		}
	}
	for _, value := range config.ProbeTargets {
		encoded, _ := json.Marshal(value)
		selectionBytes += 2*len(encoded) + 160
	}
	envelope := len(data) - selectionBytes
	t.Logf("maximum ordinary report: %d bytes; fixed envelope: %d bytes", len(data), envelope)
	if envelope > 13*1024 {
		t.Fatalf("fixed envelope exceeds reserved 13 KiB: %d", envelope)
	}
	if len(data) > v1.MaxMessageBytes {
		t.Fatalf("accepted ordinary selections produce %d-byte report", len(data))
	}
}
