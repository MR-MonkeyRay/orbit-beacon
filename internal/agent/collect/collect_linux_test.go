//go:build linux

package collect_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"orbitbeacon/internal/agent/collect"
	v1 "orbitbeacon/internal/protocol/v1"
)

func linuxConfig() v1.RuntimeSettings {
	return v1.RuntimeSettings{ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, DiskPaths: []string{"/"}, Interfaces: []string{"lo"}, ProbeTargets: []string{}}
}

func meminfo(t *testing.T) map[string]uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		values[strings.TrimSuffix(fields[0], ":")] = n * 1024
	}
	return values
}

func procLoadAndUptime(t *testing.T) ([3]float64, float64) {
	t.Helper()
	loadData, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		t.Fatal(err)
	}
	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Fatal(err)
	}
	var loads [3]float64
	for i, field := range strings.Fields(string(loadData))[:3] {
		loads[i], err = strconv.ParseFloat(field, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	uptime, err := strconv.ParseFloat(strings.Fields(string(uptimeData))[0], 64)
	if err != nil {
		t.Fatal(err)
	}
	return loads, uptime
}

func TestLinuxCollectorReadsRealHostWithoutExecutablePATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := collect.New(collect.Options{Config: linuxConfig()})
	if err != nil {
		t.Fatal(err)
	}
	before := meminfo(t)
	loadBefore, uptimeBefore := procLoadAndUptime(t)
	metrics, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after := meminfo(t)
	loadAfter, uptimeAfter := procLoadAndUptime(t)
	if metrics.MemoryTotalBytes == nil || *metrics.MemoryTotalBytes != before["MemTotal"] {
		t.Fatalf("memory total=%v want %d", metrics.MemoryTotalBytes, before["MemTotal"])
	}
	if metrics.MemoryAvailableBytes == nil || *metrics.MemoryAvailableBytes > before["MemTotal"] {
		t.Fatal("available memory missing/out of range")
	}
	if metrics.SwapTotalBytes == nil || *metrics.SwapTotalBytes != before["SwapTotal"] {
		t.Fatal("swap total differs from real procfs")
	}
	if metrics.SwapUsedBytes == nil || *metrics.SwapUsedBytes > after["SwapTotal"] {
		t.Fatal("swap used missing/out of range")
	}
	for name, value := range map[string]*float64{"load1": metrics.Load1, "load5": metrics.Load5, "load15": metrics.Load15, "uptime_seconds": metrics.UptimeSeconds} {
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || metrics.MetricStatus[name] != "ok" {
			t.Fatalf("real %s missing/invalid: %v status=%s", name, value, metrics.MetricStatus[name])
		}
	}
	for i, value := range []*float64{metrics.Load1, metrics.Load5, metrics.Load15} {
		low, high := math.Min(loadBefore[i], loadAfter[i]), math.Max(loadBefore[i], loadAfter[i])
		if *value < low-0.01 || *value > high+0.01 {
			t.Fatalf("load %d=%v outside procfs observation [%v,%v]", i, *value, low, high)
		}
	}
	if *metrics.UptimeSeconds < uptimeBefore-1 || *metrics.UptimeSeconds > uptimeAfter+1 {
		t.Fatalf("uptime=%v outside procfs [%v,%v]", *metrics.UptimeSeconds, uptimeBefore, uptimeAfter)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		t.Fatal(err)
	}
	if len(metrics.Disks) != 1 || metrics.Disks[0].Path != "/" || metrics.Disks[0].TotalBytes == nil || *metrics.Disks[0].TotalBytes != stat.Blocks*uint64(stat.Bsize) || metrics.Disks[0].UsedBytes == nil || *metrics.Disks[0].UsedBytes > *metrics.Disks[0].TotalBytes {
		t.Fatalf("disk does not match statfs: %+v", metrics.Disks)
	}
	if len(metrics.Networks) != 1 || metrics.Networks[0].Name != "lo" || metrics.Networks[0].RxBytesPerSec != nil || metrics.Networks[0].TxBytesPerSec != nil || metrics.MetricStatus["network:lo"] != "stale" {
		t.Fatalf("first network sample invented rate: %+v", metrics)
	}
	if metrics.CPUPercent != nil || metrics.MetricStatus["cpu_percent"] != "stale" {
		t.Fatal("first CPU sample invented percentage")
	}
	time.Sleep(100 * time.Millisecond)
	second, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.CPUPercent == nil || *second.CPUPercent < 0 || *second.CPUPercent > 100 || second.MetricStatus["cpu_percent"] != "ok" {
		t.Fatalf("differential CPU invalid: %+v", second)
	}
	if second.Networks[0].RxBytesPerSec == nil || second.Networks[0].TxBytesPerSec == nil {
		t.Fatal("second real network sample lacks differential rates")
	}
	for _, key := range []string{"cpu", "disk:/", "network:lo"} {
		source, ok := second.Sources[key]
		if !ok || source.SampleID == "" || source.AgeMS < 0 || source.SourceIntervalMS <= 0 {
			t.Fatalf("missing source metadata %s: %+v", key, source)
		}
	}
}

func TestLinuxReadFailuresAreNullWithStatusAndDoNotStopOtherMetrics(t *testing.T) {
	config := linuxConfig()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	config.DiskPaths = []string{missing}
	config.Interfaces = []string{"orbit-missing0"}
	c, err := collect.New(collect.Options{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.MemoryTotalBytes == nil || *metrics.MemoryTotalBytes == 0 || metrics.MetricStatus["memory_total_bytes"] != "ok" {
		t.Fatal("unavailable disk/network broke memory collection")
	}
	if len(metrics.Disks) != 1 || metrics.Disks[0].TotalBytes != nil || metrics.Disks[0].UsedBytes != nil || metrics.MetricStatus["disk:"+missing] != "error" {
		t.Fatalf("missing disk disguised as zero: %+v", metrics)
	}
	if len(metrics.Networks) != 1 || metrics.Networks[0].RxBytesPerSec != nil || metrics.Networks[0].TxBytesPerSec != nil || metrics.MetricStatus["network:orbit-missing0"] != "unsupported" {
		t.Fatalf("missing interface disguised as zero: %+v", metrics)
	}
	data, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	var disks []map[string]json.RawMessage
	if err := json.Unmarshal(object["disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if string(disks[0]["total_bytes"]) != "null" || string(disks[0]["used_bytes"]) != "null" {
		t.Fatal("unavailable disk not null on wire")
	}
}

func TestNetworkDifferentialUsesMonotonicWindowAndHandlesReset(t *testing.T) {
	var rates collect.NetworkDifferencer
	first := rates.Observe("eth0", collect.NetworkCounter{RxBytes: 100, TxBytes: 200}, time.Second)
	if first.Network.RxBytesPerSec != nil || first.Network.TxBytesPerSec != nil || first.Status != "stale" {
		t.Fatalf("first sample=%+v", first)
	}
	second := rates.Observe("eth0", collect.NetworkCounter{RxBytes: 500, TxBytes: 1000}, 3*time.Second)
	if second.Network.RxBytesPerSec == nil || *second.Network.RxBytesPerSec != 200 || second.Network.TxBytesPerSec == nil || *second.Network.TxBytesPerSec != 400 || second.Source.MeasurementWindowMS == nil || *second.Source.MeasurementWindowMS != 2000 || second.Status != "ok" {
		t.Fatalf("wrong differential=%+v", second)
	}
	if second.Source.SampleID == first.Source.SampleID {
		t.Fatal("new source sample reused id")
	}
	reset := rates.Observe("eth0", collect.NetworkCounter{RxBytes: 5, TxBytes: 10}, 4*time.Second)
	if reset.Network.RxBytesPerSec != nil || reset.Network.TxBytesPerSec != nil || reset.Status != "stale" {
		t.Fatalf("counter reset became huge/negative rate: %+v", reset)
	}
	recovered := rates.Observe("eth0", collect.NetworkCounter{RxBytes: 105, TxBytes: 210}, 5*time.Second)
	if recovered.Network.RxBytesPerSec == nil || *recovered.Network.RxBytesPerSec != 100 || recovered.Network.TxBytesPerSec == nil || *recovered.Network.TxBytesPerSec != 200 {
		t.Fatalf("reset baseline wrong: %+v", recovered)
	}
	zeroWindow := rates.Observe("eth0", collect.NetworkCounter{RxBytes: 205, TxBytes: 410}, 5*time.Second)
	if zeroWindow.Network.RxBytesPerSec != nil || zeroWindow.Status != "stale" {
		t.Fatalf("zero time window accepted: %+v", zeroWindow)
	}
	other := rates.Observe("eth1", collect.NetworkCounter{RxBytes: 10000, TxBytes: 20000}, 6*time.Second)
	if other.Network.RxBytesPerSec != nil {
		t.Fatal("interface switch reused another interface baseline")
	}
}

func TestLinuxUnavailableProcfsMetricsHaveExplicitStatusAndKeepDiskCollection(t *testing.T) {
	t.Setenv("HOST_PROC", t.TempDir())
	c, err := collect.New(collect.Options{Config: linuxConfig()})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cpu_percent", "load1", "load5", "load15", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes", "uptime_seconds"} {
		if string(object[name]) != "null" {
			t.Errorf("unreadable %s became %s instead of null", name, object[name])
		}
		switch metrics.MetricStatus[name] {
		case "error", "unsupported", "permission_denied":
		default:
			t.Errorf("unreadable %s lacks explicit failure: %q", name, metrics.MetricStatus[name])
		}
	}
	if len(metrics.Disks) != 1 || metrics.Disks[0].TotalBytes == nil || *metrics.Disks[0].TotalBytes == 0 || metrics.MetricStatus["disk:/"] != "ok" {
		t.Fatal("procfs failure stopped independent statfs collection")
	}
}
