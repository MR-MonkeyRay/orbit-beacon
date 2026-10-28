package service_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestSchedulerReportFailureLoggingTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 5))
		if err != nil {
			t.Fatal(err)
		}
		var attempts atomic.Int64
		var mu sync.Mutex
		var logs []string
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, Config: cfg, RandomFloat: func() float64 { return 0.5 },
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				attempt := attempts.Add(1)
				if attempt <= 16 || attempt == 18 {
					return v1.ReportResponse{}, &service.HTTPError{StatusCode: http.StatusServiceUnavailable, RetryAfter: "server-controlled-value"}
				}
				return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
			},
			Log: func(message string) {
				mu.Lock()
				defer mu.Unlock()
				logs = append(logs, message)
			},
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		time.Sleep(665 * time.Second)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		want := []string{
			"report failed: status=503; retrying in 1s",
			"report still failing: consecutive_failures=11; status=503; retrying in 1m0s",
			"report still failing: consecutive_failures=16; status=503; retrying in 1m0s",
			"report recovered after 16 consecutive failures",
			"report failed: status=503; retrying in 1s",
			"report recovered after 1 consecutive failures",
		}
		if len(logs) != len(want) {
			t.Fatalf("logs = %q, want %q", logs, want)
		}
		for i := range want {
			if logs[i] != want[i] {
				t.Errorf("log[%d] = %q, want %q", i, logs[i], want[i])
			}
			if strings.Contains(logs[i], "server-controlled-value") {
				t.Errorf("log exposed server-controlled text: %q", logs[i])
			}
		}
	})
}

func TestSchedulerReportFailureLoggingHTTPStatusAndBackoff(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		retryAfter string
		want       string
	}{
		{name: "bad_request", status: http.StatusBadRequest, want: "report failed: status=400; retrying in 1s"},
		{name: "unauthorized", status: http.StatusUnauthorized, want: "report failed: status=401; retrying in 1m0s"},
		{name: "rate_limited", status: http.StatusTooManyRequests, retryAfter: "17", want: "report failed: status=429; retrying in 17s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 5))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				logged := make(chan string, 1)
				scheduler := service.NewScheduler(service.SchedulerOptions{
					Identity: state, Config: cfg, RandomFloat: func() float64 { return 0.5 },
					Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
					Report: func(context.Context, v1.Report) (v1.ReportResponse, error) {
						return v1.ReportResponse{}, &service.HTTPError{StatusCode: tt.status, RetryAfter: tt.retryAfter}
					},
					Log: func(message string) { logged <- message },
				})
				done := make(chan error, 1)
				go func() { done <- scheduler.Run(ctx) }()
				synctest.Wait()
				select {
				case got := <-logged:
					if got != tt.want {
						t.Fatalf("log = %q, want %q", got, tt.want)
					}
				default:
					t.Fatal("HTTP failure was not logged")
				}
				cancel()
				synctest.Wait()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestSchedulerReportFailureLoggingClassifiesLocalErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: "report failed: error=timeout; retrying in 1s"},
		{name: "connection", err: errors.New("secret URL and bearer value"), want: "report failed: error=connection_failed; retrying in 1s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 5))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				logged := make(chan string, 1)
				scheduler := service.NewScheduler(service.SchedulerOptions{
					Identity: state, Config: cfg, RandomFloat: func() float64 { return 0.5 },
					Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
					Report:  func(context.Context, v1.Report) (v1.ReportResponse, error) { return v1.ReportResponse{}, tt.err },
					Log:     func(message string) { logged <- message },
				})
				done := make(chan error, 1)
				go func() { done <- scheduler.Run(ctx) }()
				synctest.Wait()
				select {
				case got := <-logged:
					if got != tt.want {
						t.Fatalf("log = %q, want %q", got, tt.want)
					}
				default:
					t.Fatal("first failure was not logged")
				}
				cancel()
				synctest.Wait()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestHTTPErrorIdentityConflictText(t *testing.T) {
	if got := (&service.HTTPError{StatusCode: http.StatusConflict, Code: "identity_conflict"}).Error(); got != "identity_conflict" {
		t.Fatalf("identity conflict error = %q", got)
	}
	if got := (&service.HTTPError{StatusCode: http.StatusConflict, Code: "other"}).Error(); got == "identity_conflict" {
		t.Fatalf("other conflict exposed identity conflict text: %q", got)
	}
}

func TestSchedulerCancellationDoesNotLogFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 5))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		var logs atomic.Int64
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, Config: cfg,
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report: func(ctx context.Context, _ v1.Report) (v1.ReportResponse, error) {
				<-ctx.Done()
				return v1.ReportResponse{}, ctx.Err()
			},
			Log: func(string) { logs.Add(1) },
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
		if logs.Load() != 0 {
			t.Fatalf("cancellation emitted %d report failure logs", logs.Load())
		}
	})
}

type loggingTimeoutTransport struct{}

func (loggingTimeoutTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, loggingTimeoutError{}
}

type loggingTimeoutError struct{}

func (loggingTimeoutError) Error() string { return "secret URL and bearer value" }
func (loggingTimeoutError) Timeout() bool { return true }

func TestSchedulerClientTimeoutLoggingIsSanitized(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 5))
		if err != nil {
			t.Fatal(err)
		}
		client, err := service.NewClient(service.ClientOptions{
			ServerURL:  "https://server-controlled.example",
			HTTPClient: &http.Client{Transport: loggingTimeoutTransport{}},
			Identity:   state,
			Config:     cfg,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logged := make(chan string, 1)
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Identity: state, Config: cfg, RandomFloat: func() float64 { return 0.5 },
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report:  client.Report,
			Log:     func(message string) { logged <- message },
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		select {
		case got := <-logged:
			if got != "report failed: error=timeout; retrying in 1s" {
				t.Fatalf("log = %q", got)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "example") {
				t.Fatalf("log exposed transport or URL text: %q", got)
			}
		default:
			t.Fatal("client timeout was not logged")
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
