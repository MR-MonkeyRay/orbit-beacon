//go:build linux

package collect_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/agent/collect"
	v1 "orbitbeacon/internal/protocol/v1"
)

func assertCompletePartialMetrics(t *testing.T, metrics v1.Metrics, config v1.RuntimeSettings) {
	t.Helper()
	if err := metrics.Validate(); err != nil {
		t.Errorf("partial collection is not protocol-valid: %v", err)
	}
	for _, field := range []string{"cpu_percent", "load1", "load5", "load15", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes", "uptime_seconds"} {
		if metrics.MetricStatus[field] == "" {
			t.Errorf("partial collection omitted scalar status %s", field)
		}
	}
	for _, family := range []string{"cpu", "load", "memory", "swap", "uptime"} {
		if source, ok := metrics.Sources[family]; !ok || source.Validate() != nil {
			t.Errorf("partial collection omitted valid source %s", family)
		}
	}
	if len(metrics.Disks) != len(config.DiskPaths) || len(metrics.Networks) != len(config.Interfaces) {
		t.Errorf("partial collection lost configured series: disks=%+v networks=%+v", metrics.Disks, metrics.Networks)
	}
	for i, disk := range metrics.Disks {
		if i < len(config.DiskPaths) && disk.Path != config.DiskPaths[i] {
			t.Errorf("partial collection changed disk selection: %+v", metrics.Disks)
		}
	}
	for i, network := range metrics.Networks {
		if i < len(config.Interfaces) && network.Name != config.Interfaces[i] {
			t.Errorf("partial collection changed interface selection: %+v", metrics.Networks)
		}
	}
	for _, key := range []string{"cpu", "network:lo", "network:orbit-missing0"} {
		source, ok := metrics.Sources[key]
		if !ok || source.MeasurementWindowMS == nil || *source.MeasurementWindowMS < 0 {
			t.Errorf("partial collection omitted nonnegative measurement window for %s", key)
		}
	}
}

func TestCollectorDeadlineReturnsCompletePartialMetricsAndError(t *testing.T) {
	for _, warmed := range []bool{false, true} {
		name := "cold collector"
		if warmed {
			name = "collector with cached samples"
		}
		t.Run(name, func(t *testing.T) {
			config := linuxConfig()
			config.Interfaces = []string{"lo", "orbit-missing0"}
			config.DiskPaths = []string{"/", t.TempDir()}
			collector, err := collect.New(collect.Options{Config: config})
			if err != nil {
				t.Fatal(err)
			}
			if warmed {
				if _, err := collector.Collect(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			metrics, err := collector.Collect(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("collection deadline error=%v", err)
			}
			assertCompletePartialMetrics(t, metrics, config)
		})
	}
}

func TestCollectorUnavailableAndWarmupSourcesAlwaysCarryMeasurementWindows(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "warm-up"
		if unavailable {
			name = "unavailable procfs"
		}
		t.Run(name, func(t *testing.T) {
			if unavailable {
				t.Setenv("HOST_PROC", t.TempDir())
			}
			config := linuxConfig()
			config.Interfaces = []string{"lo", "orbit-missing0"}
			collector, err := collect.New(collect.Options{Config: config})
			if err != nil {
				t.Fatal(err)
			}
			metrics, err := collector.Collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertCompletePartialMetrics(t, metrics, config)
		})
	}
}

func TestUnavailableAutomaticNetworkHasMeasurementWindow(t *testing.T) {
	t.Setenv("HOST_PROC", t.TempDir())
	config := linuxConfig()
	config.Interfaces = []string{}
	collector, err := collect.New(collect.Options{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics.Networks) != 1 || metrics.Networks[0].Name != "default" {
		t.Fatalf("missing automatic route did not retain unavailable network series: %+v", metrics.Networks)
	}
	source, ok := metrics.Sources["network:default"]
	if !ok || source.MeasurementWindowMS == nil || *source.MeasurementWindowMS < 0 {
		t.Error("unavailable automatic network omitted nonnegative measurement window")
	}
	if err := metrics.Validate(); err != nil {
		t.Errorf("unavailable automatic network metrics invalid: %v", err)
	}
}
