package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestSchedulerReloadAppliesAtSampleBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		settings := v1.DefaultRuntimeSettings()
		reload := make(chan struct{}, 1)
		var collections atomic.Int64
		reports := make(chan v1.Report, 8)
		var reject bool
		candidate := settings
		logs := &testLogSink{}
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Logger:   logs.logger(),
			Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return settings }, Reload: reload,
			ReloadConfig: func(context.Context) error {
				if reject {
					return errors.New("invalid settings")
				}
				settings = candidate
				return nil
			},
			Collect: func(context.Context) (v1.Metrics, error) { collections.Add(1); return schedulerMetrics(), nil },
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				reports <- report
				return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
			},
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		first := <-reports
		reload <- struct{}{}
		synctest.Wait()
		if collections.Load() != 1 {
			t.Fatal("unchanged configuration caused a sample")
		}
		time.Sleep(2 * time.Second)
		candidate.ReportIntervalSeconds = 5
		candidate.ProbeTargets = []string{"https://example.test/health"}
		reload <- struct{}{}
		synctest.Wait()
		second := <-reports
		if second.Runtime.ReportIntervalSeconds != 5 || len(second.Runtime.ProbeTargets) != 1 || second.Seq != 2 || second.RunGeneration != first.RunGeneration || second.SessionID != first.SessionID {
			t.Fatalf("reload report = %+v", second)
		}
		reject = true
		reload <- struct{}{}
		synctest.Wait()
		if collections.Load() != 2 {
			t.Fatal("rejected reload caused a sample")
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		third := <-reports
		if third.Runtime.ReportIntervalSeconds != 5 || third.Seq != 3 {
			t.Fatalf("reload changed cadence or snapshot: %+v", third)
		}
		gotLogs := logs.snapshot()
		messages := make([]string, len(gotLogs))
		for i, record := range gotLogs {
			messages[i] = record.message
		}
		if !reflect.DeepEqual(messages, []string{"configuration_unchanged", "configuration_reloaded", "configuration_reload_rejected"}) {
			t.Fatalf("reload logs = %+v", gotLogs)
		}
		if gotLogs[2].attrs["error_code"] != "configuration_reload_rejected" {
			t.Fatalf("rejected reload attrs = %+v", gotLogs[2].attrs)
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestSchedulerReloadWaitsForCollectionAndKeepsSnapshotRuntime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		settings := v1.DefaultRuntimeSettings()
		settings.ProbeTargets = []string{"tcp://old.example:80"}
		reload := make(chan struct{}, 1)
		release := make(chan struct{})
		reports := make(chan v1.Report, 8)
		var collections, loads atomic.Int64
		var collecting atomic.Bool
		var requestedTargets [][]string
		probes := snapshotProbeReporter{results: func(targets []string) []v1.ProbeResult {
			requestedTargets = append(requestedTargets, append([]string{}, targets...))
			return nil
		}}
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return settings }, Reload: reload,
			ReloadConfig: func(context.Context) error {
				if collecting.Load() {
					t.Error("reload overlapped collection")
				}
				loads.Add(1)
				settings = v1.DefaultRuntimeSettings()
				settings.ReportIntervalSeconds = 5
				settings.ProbeTargets = []string{"tcp://new.example:443"}
				return nil
			},
			Collect: func(ctx context.Context) (v1.Metrics, error) {
				collecting.Store(true)
				defer collecting.Store(false)
				if collections.Add(1) == 1 {
					select {
					case <-release:
					case <-ctx.Done():
						return schedulerMetrics(), ctx.Err()
					}
				}
				return schedulerMetrics(), nil
			},
			Probes: probes,
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				reports <- report
				return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
			},
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		reload <- struct{}{}
		synctest.Wait()
		if loads.Load() != 0 {
			t.Fatal("reload ran during blocked collection")
		}
		close(release)
		synctest.Wait()
		if loads.Load() != 1 || collections.Load() != 2 {
			t.Fatalf("boundary reload loads=%d samples=%d", loads.Load(), collections.Load())
		}
		// 上报任务可能合并第一个通知，但每份快照都必须使用自己的目标集，
		// 不能使用更新后的全局配置。
		for len(reports) > 0 {
			report := <-reports
			targets := requestedTargets[0]
			requestedTargets = requestedTargets[1:]
			if !reflect.DeepEqual(targets, report.Runtime.ProbeTargets) {
				t.Fatalf("worker mixed runtime and probes: %+v / %v", report.Runtime, targets)
			}
			if report.Seq == 1 && report.Runtime.ReportIntervalSeconds != 10 {
				t.Fatal("old sample borrowed new runtime")
			}
			if report.Seq == 2 && report.Runtime.ReportIntervalSeconds != 5 {
				t.Fatal("new sample used old runtime")
			}
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

type snapshotProbeReporter struct {
	results func([]string) []v1.ProbeResult
}

func (p snapshotProbeReporter) Results(targets []string) []v1.ProbeResult { return p.results(targets) }

func TestSchedulerWorkerUsesCapturedReportProbeTargets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		settings := v1.DefaultRuntimeSettings()
		settings.ProbeTargets = []string{"tcp://old.example:80"}
		reload := make(chan struct{}, 1)
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		reports := make(chan v1.Report, 4)
		requested := make(chan []string, 4)
		var entries atomic.Int64
		ctx, cancel := context.WithCancel(t.Context())
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return settings }, Reload: reload,
			ReloadConfig: func(context.Context) error {
				settings = v1.DefaultRuntimeSettings()
				settings.ProbeTargets = []string{"tcp://new.example:443"}
				return nil
			},
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Traffic: snapshotTrafficReporter{entries: func() []v1.Traffic {
				if entries.Add(1) == 1 {
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return nil
			}},
			Probes: snapshotProbeReporter{results: func(targets []string) []v1.ProbeResult { requested <- targets; return nil }},
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				reports <- report
				return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
			},
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		<-entered
		reload <- struct{}{}
		synctest.Wait()
		close(release)
		synctest.Wait()
		first := <-reports
		targets := <-requested
		if first.Seq != 1 || !reflect.DeepEqual(targets, []string{"tcp://old.example:80"}) || !reflect.DeepEqual(first.Runtime.ProbeTargets, targets) {
			t.Fatalf("old report borrowed new global target selection: %+v / %v", first.Runtime, targets)
		}
		second := <-reports
		targets = <-requested
		if second.Seq != 2 || !reflect.DeepEqual(targets, []string{"tcp://new.example:443"}) || !reflect.DeepEqual(second.Runtime.ProbeTargets, targets) {
			t.Fatalf("new report used wrong target selection: %+v / %v", second.Runtime, targets)
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

type snapshotTrafficReporter struct{ entries func() []v1.Traffic }

func (t snapshotTrafficReporter) Entries() []v1.Traffic  { return t.entries() }
func (snapshotTrafficReporter) Acknowledge([]v1.Traffic) {}
