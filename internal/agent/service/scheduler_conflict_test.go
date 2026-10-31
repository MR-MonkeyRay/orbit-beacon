package service_test

import (
	"context"
	"errors"
	"fmt"
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

func TestSchedulerIdentityConflictStopsRun(t *testing.T) {
	for _, activeCollection := range []bool{false, true} {
		name := "waiting_for_collection"
		if activeCollection {
			name = "collection_in_flight"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
				if err != nil {
					t.Fatal(err)
				}
				cfg := runtimeSettings(5)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				conflict := &service.HTTPError{StatusCode: http.StatusConflict, Code: "identity_conflict"}
				failure := fmt.Errorf("report rejected: %w", conflict)
				release := make(chan struct{})
				var collections, activeCollections, reports, activeReports atomic.Int64
				var reportContext context.Context
				logs := &testLogSink{}
				scheduler := service.NewScheduler(service.SchedulerOptions{Logger: logs.logger(), Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, RandomFloat: func() float64 { return 0.5 },
					Collect: func(ctx context.Context) (v1.Metrics, error) {
						activeCollections.Add(1)
						defer activeCollections.Add(-1)
						if collections.Add(1) > 1 && activeCollection {
							<-ctx.Done()
							return v1.Metrics{}, ctx.Err()
						}
						return schedulerMetrics(), nil
					},
					Report: func(ctx context.Context, _ v1.Report) (v1.ReportResponse, error) {
						reports.Add(1)
						activeReports.Add(1)
						defer activeReports.Add(-1)
						reportContext = ctx
						select {
						case <-release:
							return v1.ReportResponse{}, failure
						case <-ctx.Done():
							return v1.ReportResponse{}, ctx.Err()
						}
					}})
				done := make(chan error, 1)
				go func() { done <- scheduler.Run(ctx) }()
				synctest.Wait()
				wantCollections := int64(1)
				if activeCollection {
					time.Sleep(5 * time.Second)
					synctest.Wait()
					wantCollections = 2
					if activeCollections.Load() != 1 {
						t.Fatal("second collection did not start before identity conflict")
					}
				}
				close(release)
				synctest.Wait()
				select {
				case err := <-done:
					if !errors.Is(err, conflict) {
						t.Fatalf("Run error = %v, want original identity conflict", err)
					}
				default:
					t.Fatal("scheduler kept running after terminal identity conflict")
				}
				if reportContext == nil || !errors.Is(reportContext.Err(), context.Canceled) {
					t.Fatal("scheduler did not cancel its worker context")
				}
				if activeCollections.Load() != 0 || activeReports.Load() != 0 {
					t.Fatal("scheduler returned before collection/report work drained")
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if collections.Load() != wantCollections || reports.Load() != 1 {
					t.Fatalf("work continued after identity conflict: collections=%d reports=%d", collections.Load(), reports.Load())
				}
				for _, record := range logs.snapshot() {
					if record.message == "credentials_invalid" {
						t.Fatal("identity conflict was incorrectly treated as invalid credentials")
					}
				}
			})
		})
	}
}

func TestSchedulerOtherReportFailuresRemainRetryable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		code   string
	}{
		{name: "other_conflict", status: http.StatusConflict, code: "config_conflict"},
		{name: "unclassified_conflict", status: http.StatusConflict},
		{name: "transient_status", status: http.StatusServiceUnavailable, code: "identity_conflict"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
				if err != nil {
					t.Fatal(err)
				}
				cfg := runtimeSettings(5)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var collections, reports atomic.Int64
				scheduler := service.NewScheduler(service.SchedulerOptions{Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, RandomFloat: func() float64 { return 0.5 },
					Collect: func(context.Context) (v1.Metrics, error) {
						collections.Add(1)
						return schedulerMetrics(), nil
					},
					Report: func(context.Context, v1.Report) (v1.ReportResponse, error) {
						reports.Add(1)
						return v1.ReportResponse{}, &service.HTTPError{StatusCode: tt.status, Code: tt.code}
					}})
				done := make(chan error, 1)
				go func() { done <- scheduler.Run(ctx) }()
				synctest.Wait()
				time.Sleep(5 * time.Second)
				synctest.Wait()
				if collections.Load() != 2 || reports.Load() != 3 {
					t.Fatalf("retry behavior changed: collections=%d reports=%d", collections.Load(), reports.Load())
				}
				select {
				case err := <-done:
					t.Fatalf("retryable failure stopped scheduler: %v", err)
				default:
				}
				cancel()
				synctest.Wait()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("Run error = %v, want context cancellation", err)
				}
				beforeCollections, beforeReports := collections.Load(), reports.Load()
				time.Sleep(time.Minute)
				synctest.Wait()
				if collections.Load() != beforeCollections || reports.Load() != beforeReports {
					t.Fatal("scheduler continued collection/report work after cancellation")
				}
			})
		})
	}
}
