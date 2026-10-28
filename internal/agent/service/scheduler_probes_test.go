package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/probe"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestSchedulerSendsLatestProbesOnRetryAndDropsRemovedTargets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		initial := runtimeSettings(5)
		initial.ProbeTargets = []string{"tcp://example.com:80"}
		runner := probe.NewRunner(probe.Options{Check: func(ctx context.Context, target string) v1.ProbeResult {
			time.Sleep(500 * time.Millisecond)
			latency := 2.0
			return v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, CheckedAt: time.Now().UTC()}
		}})
		if err := runner.SetTargets(initial.ProbeTargets); err != nil {
			t.Fatal(err)
		}
		reload := make(chan struct{}, 1)
		reports := make(chan v1.Report, 10)
		attempt := 0
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return initial }, Probes: runner, Reload: reload, ReloadConfig: func(context.Context) error {
				initial = runtimeSettings(5)
				return runner.SetTargets(nil)
			}, RandomFloat: func() float64 { return .5 },
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				reports <- report
				attempt++
				if attempt == 1 {
					return v1.ReportResponse{}, errors.New("offline")
				}
				return v1.ReportResponse{}, nil
			},
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		probesDone := make(chan error, 1)
		go func() { probesDone <- runner.Run(ctx) }()
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		first := <-reports
		if len(first.ProbeResults) != 0 {
			t.Fatal("report waited for unfinished probe")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		retry := <-reports
		if retry.Seq != first.Seq || len(retry.ProbeResults) != 1 {
			t.Fatalf("retry missing latest probes: %+v", retry.ProbeResults)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		next := <-reports
		if len(next.ProbeResults) != 1 || !next.ProbeResults[0].CheckedAt.Equal(retry.ProbeResults[0].CheckedAt) {
			t.Fatal("report did not repeat latest completed check")
		}
		reload <- struct{}{}
		synctest.Wait()
		next = <-reports
		if len(next.ProbeResults) != 0 {
			t.Fatal("removed target in report")
		}
		cancel()
		synctest.Wait()
		<-probesDone
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
