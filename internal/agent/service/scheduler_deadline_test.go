package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestSchedulerDeadlineStillReportsCompleteConfiguredMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		config := desired(1, 10)
		config.Interfaces = []string{"deadline0", "deadline1"}
		config.DiskPaths = []string{t.TempDir(), t.TempDir()}
		cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), config)
		if err != nil {
			t.Fatal(err)
		}
		collector, err := collect.New(collect.Options{Config: config})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reports := make(chan v1.Report, 1)
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, Config: cfg,
			Collect: func(ctx context.Context) (v1.Metrics, error) {
				// Exhaust the scheduler's real collection deadline before collection can finish.
				<-ctx.Done()
				return collector.Collect(ctx)
			},
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				reports <- report
				return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
			},
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		var report v1.Report
		select {
		case report = <-reports:
		default:
			t.Fatal("collection deadline prevented scheduler from reporting")
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := report.Validate(); err != nil {
			t.Errorf("deadline report is not protocol-valid: %v", err)
		}
		if report.Seq != 1 || report.ConfigRevision != config.Revision {
			t.Errorf("deadline changed report ownership/revision: %+v", report)
		}
		for _, field := range []string{"cpu_percent", "load1", "load5", "load15", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes", "uptime_seconds"} {
			if report.Metrics.MetricStatus[field] == "" {
				t.Errorf("deadline report missing scalar status %s", field)
			}
		}
		for _, family := range []string{"cpu", "load", "memory", "swap", "uptime"} {
			if source, ok := report.Metrics.Sources[family]; !ok || source.Validate() != nil {
				t.Errorf("deadline report missing valid source %s", family)
			}
		}
		if len(report.Metrics.Disks) != len(config.DiskPaths) || len(report.Metrics.Networks) != len(config.Interfaces) {
			t.Errorf("deadline report omitted configured series: %+v", report.Metrics)
		}
		for _, path := range config.DiskPaths {
			found := false
			for _, disk := range report.Metrics.Disks {
				if disk.Path == path {
					found = true
				}
			}
			if !found {
				t.Errorf("deadline report missing disk %s", path)
			}
		}
		for _, name := range config.Interfaces {
			found := false
			for _, network := range report.Metrics.Networks {
				if network.Name == name {
					found = true
				}
			}
			if !found {
				t.Errorf("deadline report missing interface %s", name)
			}
			if source := report.Metrics.Sources["network:"+name]; source.MeasurementWindowMS == nil || *source.MeasurementWindowMS < 0 {
				t.Errorf("deadline network %s missing measurement window", name)
			}
		}
		if source := report.Metrics.Sources["cpu"]; source.MeasurementWindowMS == nil || *source.MeasurementWindowMS < 0 {
			t.Error("deadline CPU missing measurement window")
		}
	})
}
