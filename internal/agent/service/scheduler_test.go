package service_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func schedulerMetrics() v1.Metrics {
	statuses := make(map[string]string)
	for _, key := range []string{"cpu_percent", "load1", "load5", "load15", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes", "uptime_seconds"} {
		statuses[key] = "disabled"
	}
	sources := make(map[string]v1.SourceMetadata)
	for _, key := range []string{"cpu", "load", "memory", "swap", "uptime"} {
		sources[key] = v1.SourceMetadata{SampleID: key + "-test", SourceIntervalMS: 10000}
	}
	window := int64(0)
	source := sources["cpu"]
	source.MeasurementWindowMS = &window
	sources["cpu"] = source
	return v1.Metrics{MetricStatus: statuses, Sources: sources, Disks: []v1.DiskMetrics{}, Networks: []v1.NetworkMetrics{}}
}

func TestBackoffSequenceJitterRetryAfterAndCredentials(t *testing.T) {
	for _, random := range []float64{0, 0.5, 1} {
		b := service.NewBackoff(func() float64 { return random })
		for _, seconds := range []int{1, 2, 4, 8, 16, 32, 60, 60, 60} {
			got := b.Next(http.StatusServiceUnavailable, "", time.Unix(0, 0))
			base := time.Duration(seconds) * time.Second
			if got < base*8/10 || got > base*12/10 {
				t.Fatalf("random=%v base=%s backoff=%s", random, base, got)
			}
			if random == 0.5 && got != base {
				t.Fatalf("central jitter changed base %s to %s", base, got)
			}
		}
		b.Reset()
		if got := b.Next(0, "", time.Unix(0, 0)); got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("success did not reset backoff: %s", got)
		}
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for _, tt := range []struct {
			header string
			want   time.Duration
		}{
			{"0", time.Second}, {"-5", time.Second}, {"17", 17 * time.Second}, {"9999999", 300 * time.Second},
			{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
			{now.Add(-time.Minute).Format(http.TimeFormat), time.Second},
			{now.Add(time.Hour).Format(http.TimeFormat), 300 * time.Second},
		} {
			b := service.NewBackoff(func() float64 { return 0.5 })
			if got := b.Next(status, tt.header, now); got != tt.want {
				t.Errorf("status=%d Retry-After=%q delay=%s want=%s", status, tt.header, got, tt.want)
			}
		}
	}
	b := service.NewBackoff(func() float64 { return 0 })
	for range 4 {
		if got := b.Next(http.StatusUnauthorized, "1", now); got != 60*time.Second {
			t.Fatalf("401 retry=%s want 60s", got)
		}
	}
	b.Reset()
	if got := b.Next(http.StatusBadGateway, "300", now); got != 800*time.Millisecond {
		t.Fatalf("Retry-After honored on unsupported status: %s", got)
	}
}

func TestSchedulerKeepsCollectingWithOneInFlightAndLatestOnlyNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 5))
		if err != nil {
			t.Fatal(err)
		}
		var collections, activeCollections, maxCollections, reports, activeReports, maxReports atomic.Int64
		started := make(chan v1.Report, 4)
		release := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, Config: cfg, RandomFloat: func() float64 { return 0.5 },
			Collect: func(ctx context.Context) (v1.Metrics, error) {
				active := activeCollections.Add(1)
				if active > maxCollections.Load() {
					maxCollections.Store(active)
				}
				defer activeCollections.Add(-1)
				n := collections.Add(1)
				cpu := float64(n)
				metrics := schedulerMetrics()
				metrics.CPUPercent = &cpu
				metrics.MetricStatus["cpu_percent"] = "ok"
				return metrics, nil
			},
			Report: func(ctx context.Context, report v1.Report) (v1.ReportResponse, error) {
				active := activeReports.Add(1)
				if active > maxReports.Load() {
					maxReports.Store(active)
				}
				defer activeReports.Add(-1)
				reports.Add(1)
				started <- report
				select {
				case <-release:
					return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
				case <-ctx.Done():
					return v1.ReportResponse{}, ctx.Err()
				}
			},
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		first := <-started
		if first.Seq != 1 || collections.Load() != 1 {
			t.Fatalf("first immediate sample seq=%d count=%d", first.Seq, collections.Load())
		}
		time.Sleep(100 * time.Second)
		synctest.Wait()
		if collections.Load() != 21 || reports.Load() != 1 || maxReports.Load() != 1 || maxCollections.Load() != 1 {
			t.Fatalf("blocked report stopped/unbounded work: collections=%d reports=%d report concurrency=%d collection concurrency=%d", collections.Load(), reports.Load(), maxReports.Load(), maxCollections.Load())
		}
		release <- struct{}{}
		synctest.Wait()
		latest := <-started
		if latest.Seq != 21 || latest.Metrics.CPUPercent == nil || *latest.Metrics.CPUPercent != 21 {
			t.Fatalf("worker drained obsolete offline queue instead of latest: %+v", latest)
		}
		if latest.RunGeneration != first.RunGeneration || latest.SessionID != first.SessionID || state.Current().RunGeneration != 1 {
			t.Fatal("routine samples/retries created another run generation/session")
		}
		release <- struct{}{}
		synctest.Wait()
		if reports.Load() != 2 {
			t.Fatalf("pending notifications exceeded capacity one: reports=%d", reports.Load())
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if activeCollections.Load() != 0 || activeReports.Load() != 0 {
			t.Fatal("scheduler leaked running work after cancellation")
		}
	})
}

func TestSchedulerNetworkBackoffDoesNotPauseCollection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 5))
		if err != nil {
			t.Fatal(err)
		}
		var collections atomic.Int64
		var times []time.Time
		var generations []uint64
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, Config: cfg, RandomFloat: func() float64 { return 0.5 },
			Collect: func(context.Context) (v1.Metrics, error) { collections.Add(1); return schedulerMetrics(), nil },
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				times = append(times, time.Now())
				generations = append(generations, report.RunGeneration)
				return v1.ReportResponse{}, errors.New("offline")
			},
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		time.Sleep(31 * time.Second)
		synctest.Wait()
		if collections.Load() != 7 {
			t.Fatalf("network retry paused collection: %d", collections.Load())
		}
		if len(times) != 6 {
			t.Fatalf("retry count=%d want 6", len(times))
		}
		for i, delta := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
			if times[i+1].Sub(times[i]) != delta {
				t.Fatalf("retry %d gap=%s want=%s", i, times[i+1].Sub(times[i]), delta)
			}
		}
		for _, generation := range generations {
			if generation != 1 {
				t.Fatal("retry incremented run generation")
			}
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
