//go:build linux

package collect_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/agent/collect"
)

func TestLinuxUptimeUsesSelectedHostProcFixture(t *testing.T) {
	proc := t.TempDir()
	t.Setenv("HOST_PROC", proc)
	if err := os.WriteFile(filepath.Join(proc, "uptime"), []byte("123.5 0.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	collector, err := collect.New(collect.Options{Config: linuxConfig()})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.UptimeSeconds == nil || math.Abs(*metrics.UptimeSeconds-123.5) > 0.0001 || metrics.MetricStatus["uptime_seconds"] != "ok" {
		t.Fatalf("uptime did not use HOST_PROC fixture: value=%v status=%s", metrics.UptimeSeconds, metrics.MetricStatus["uptime_seconds"])
	}
}

func TestLinuxUptimeRejectsInvalidProcValues(t *testing.T) {
	for _, value := range []string{"nan 0\n", "-1 0\n", "Inf 0\n", "not-a-number\n", "18446744073709551616 0\n"} {
		t.Run(value, func(t *testing.T) {
			proc := t.TempDir()
			t.Setenv("HOST_PROC", proc)
			if err := os.WriteFile(filepath.Join(proc, "uptime"), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			collector, err := collect.New(collect.Options{Config: linuxConfig()})
			if err != nil {
				t.Fatal(err)
			}
			metrics, err := collector.Collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if metrics.UptimeSeconds != nil || metrics.MetricStatus["uptime_seconds"] == "ok" {
				t.Fatalf("invalid proc uptime was accepted: value=%v status=%s", metrics.UptimeSeconds, metrics.MetricStatus["uptime_seconds"])
			}
		})
	}
}
