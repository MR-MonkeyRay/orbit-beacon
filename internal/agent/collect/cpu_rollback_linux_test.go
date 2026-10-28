//go:build linux

package collect

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestCPUCountersContinuous(t *testing.T) {
	base := cpu.TimesStat{User: 100, System: 20, Idle: 800, Nice: 10, Iowait: 100, Irq: 2, Softirq: 3, Steal: 4, Guest: 5, GuestNice: 6}
	for _, tt := range []struct {
		name    string
		current cpu.TimesStat
		want    bool
	}{
		{
			name:    "iowait decrease offset by idle growth",
			current: cpu.TimesStat{User: 150, System: 20, Idle: 950, Nice: 10, Iowait: 0, Irq: 2, Softirq: 3, Steal: 4, Guest: 5, GuestNice: 6},
			want:    true,
		},
		{
			name:    "uncompensated iowait decrease",
			current: cpu.TimesStat{User: 150, System: 20, Idle: 850, Nice: 10, Iowait: 0, Irq: 2, Softirq: 3, Steal: 4, Guest: 5, GuestNice: 6},
		},
		{
			name:    "other counter reset",
			current: cpu.TimesStat{User: 99, System: 20, Idle: 950, Nice: 10, Iowait: 0, Irq: 2, Softirq: 3, Steal: 4, Guest: 5, GuestNice: 6},
		},
		{
			name:    "negative iowait",
			current: cpu.TimesStat{User: 150, System: 20, Idle: 1051, Nice: 10, Iowait: -1, Irq: 2, Softirq: 3, Steal: 4, Guest: 5, GuestNice: 6},
		},
		{
			name:    "nonfinite idle",
			current: cpu.TimesStat{User: 150, System: 20, Idle: math.Inf(1), Nice: 10, Iowait: 0, Irq: 2, Softirq: 3, Steal: 4, Guest: 5, GuestNice: 6},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := cpuCountersContinuous(base, tt.current); got != tt.want {
				t.Fatalf("cpuCountersContinuous() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUTimesWithIowaitDecrease(t *testing.T) {
	for _, tt := range []struct {
		name              string
		previous, current cpu.TimesStat
		wantTotalDelta    float64
		wantBusyDelta     float64
	}{
		{
			name:           "idle growth offsets iowait decrease",
			previous:       cpu.TimesStat{User: 100, Idle: 800, Iowait: 100},
			current:        cpu.TimesStat{User: 150, Idle: 950},
			wantTotalDelta: 100,
			wantBusyDelta:  50,
		},
		{
			name:           "idle and iowait growth remain nonbusy",
			previous:       cpu.TimesStat{User: 100, Idle: 800, Iowait: 100},
			current:        cpu.TimesStat{User: 100, Idle: 850, Iowait: 150},
			wantTotalDelta: 100,
			wantBusyDelta:  0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previousTotal, previousBusy := cpuTimes(tt.previous)
			currentTotal, currentBusy := cpuTimes(tt.current)
			if got := currentTotal - previousTotal; got != tt.wantTotalDelta {
				t.Errorf("total delta = %v, want %v", got, tt.wantTotalDelta)
			}
			if got := currentBusy - previousBusy; got != tt.wantBusyDelta {
				t.Errorf("busy delta = %v, want %v", got, tt.wantBusyDelta)
			}
		})
	}
}

func TestCPUCounterRollbackIsStaleAndNextContinuousWindowRecovers(t *testing.T) {
	for _, scenario := range []struct {
		name, first, rollback, recovered string
	}{
		{"iowait rollback masks 200 percent", "1000 0 0 8000 1000 0 0 0 0 0", "2000 0 0 8500 0 0 0 0 0 0", "2500 0 0 9000 0 0 0 0 0 0"},
		{"busy component rollback hidden by another", "1000 0 1000 8000 0 0 0 0 0 0", "900 0 1200 8100 0 0 0 0 0 0", "1400 0 1200 8600 0 0 0 0 0 0"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			proc := t.TempDir()
			t.Setenv("HOST_PROC", proc)
			synctest.Test(t, func(t *testing.T) {
				collector, err := New(Options{Config: v1.DesiredConfig{Revision: 1, ReportIntervalSeconds: 5, DiskIntervalSeconds: 60, BillingDay: 1, QuotaBytes: "0"}})
				if err != nil {
					t.Fatal(err)
				}
				read := func(counters string) v1.Metrics {
					t.Helper()
					if err := os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu "+counters+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					metrics := v1.Metrics{MetricStatus: make(map[string]string), Sources: make(map[string]v1.SourceMetadata)}
					collector.collectCPU(context.Background(), &metrics)
					return metrics
				}
				read(scenario.first)
				time.Sleep(time.Second)
				invalid := read(scenario.rollback)
				if invalid.CPUPercent != nil || invalid.MetricStatus["cpu_percent"] != "stale" {
					t.Fatalf("counter rollback was presented as a trustworthy percentage: percent=%v status=%q", invalid.CPUPercent, invalid.MetricStatus["cpu_percent"])
				}
				if window := invalid.Sources["cpu"].MeasurementWindowMS; window == nil || *window != 1000 {
					t.Fatalf("rollback lost its elapsed differential window: %v", window)
				}
				time.Sleep(time.Second)
				recovered := read(scenario.recovered)
				if recovered.CPUPercent == nil || *recovered.CPUPercent != 50 || recovered.MetricStatus["cpu_percent"] != "ok" {
					t.Fatalf("new continuous baseline did not recover to 50%%: percent=%v status=%q", recovered.CPUPercent, recovered.MetricStatus["cpu_percent"])
				}
			})
		})
	}
}
