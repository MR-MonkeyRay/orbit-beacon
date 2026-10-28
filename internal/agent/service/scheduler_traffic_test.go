package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestSchedulerSendsTrafficAndAppliesOnlySuccessfulAcks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg := runtimeSettings(5)
		var samples atomic.Int64
		base := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
		traffic := collect.NewTrafficAccumulator(func() time.Time { return base.Add(time.Duration(samples.Load()-1) * time.Hour) })
		reports := make(chan v1.Report, 16)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attempt := 0
		scheduler := service.NewScheduler(service.SchedulerOptions{Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, Traffic: traffic, RandomFloat: func() float64 { return 0.5 },
			Collect: func(context.Context) (v1.Metrics, error) {
				n := samples.Add(1)
				traffic.Observe([]string{"eth0"}, map[string]collect.NetworkCounter{"eth0": {RxBytes: uint64(100 * n), TxBytes: uint64(200 * n)}})
				return schedulerMetrics(), nil
			},
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				attempt++
				reports <- report
				if attempt == 1 {
					return v1.ReportResponse{TrafficAcks: report.Traffic}, errors.New("offline")
				}
				if report.Seq == 1 { // Older servers omit acks, including on retry.
					return v1.ReportResponse{}, nil
				}
				if report.Seq == 2 {
					acks := append([]v1.Traffic(nil), report.Traffic...)
					acks[1].RxBytes = "99" // Too old to acknowledge the 100-byte bucket.
					return v1.ReportResponse{TrafficAcks: acks}, nil
				}
				return v1.ReportResponse{TrafficAcks: report.Traffic}, nil
			}})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		first := <-reports
		if len(first.Traffic) != 1 || first.Traffic[0].RxBytes != "0" {
			t.Fatalf("first report missing traffic baseline: %+v", first.Traffic)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		retry := <-reports
		if retry.Seq != first.Seq || len(retry.Traffic) != 1 || retry.Traffic[0] != first.Traffic[0] {
			t.Fatalf("retry lost cumulative traffic: %+v", retry)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		second := <-reports
		if len(second.Traffic) != 2 || second.Traffic[0] != first.Traffic[0] || second.Traffic[1].RxBytes != "100" || second.Traffic[1].TxBytes != "200" {
			t.Fatalf("failed-response ack or missing ack settled traffic: %+v", second.Traffic)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		third := <-reports
		if len(third.Traffic) != 2 || third.Traffic[0] != second.Traffic[1] {
			t.Fatalf("exact closed ack ignored or stale ack settled: %+v", third.Traffic)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		fourth := <-reports
		if len(fourth.Traffic) != 1 || !fourth.Traffic[0].PeriodStart.Equal(base.Add(3*time.Hour)) {
			t.Fatalf("successful acks not applied before next send: %+v", fourth.Traffic)
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
