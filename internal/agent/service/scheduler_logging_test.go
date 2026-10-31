package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

type testLogRecord struct {
	message string
	attrs   map[string]any
}

type testLogSink struct {
	mu      sync.Mutex
	records []testLogRecord
}

func (s *testLogSink) logger() *slog.Logger { return slog.New(testLogHandler{sink: s}) }

func (s *testLogSink) snapshot() []testLogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]testLogRecord(nil), s.records...)
}

type testLogHandler struct{ sink *testLogSink }

func (testLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h testLogHandler) Handle(_ context.Context, record slog.Record) error {
	entry := testLogRecord{message: record.Message, attrs: make(map[string]any)}
	record.Attrs(func(attr slog.Attr) bool {
		entry.attrs[attr.Key] = attr.Value.Any()
		return true
	})
	h.sink.mu.Lock()
	h.sink.records = append(h.sink.records, entry)
	h.sink.mu.Unlock()
	return nil
}
func (h testLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h testLogHandler) WithGroup(string) slog.Handler      { return h }

func TestSchedulerReportFailureLoggingTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg := runtimeSettings(5)
		var attempts atomic.Int64
		logs := &testLogSink{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		scheduler := service.NewScheduler(service.SchedulerOptions{Logger: logs.logger(), Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, RandomFloat: func() float64 { return 0.5 },
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				attempt := attempts.Add(1)
				if attempt <= 16 || attempt == 18 {
					return v1.ReportResponse{}, &service.HTTPError{StatusCode: http.StatusServiceUnavailable, RetryAfter: "server-controlled-value"}
				}
				return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
			}})
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
		got := logs.snapshot()
		want := []string{
			"report_delivery_failed",
			"report_delivery_still_failing",
			"report_delivery_still_failing",
			"report_delivery_recovered",
			"report_delivery_failed",
			"report_delivery_recovered",
		}
		if len(got) != len(want) {
			t.Fatalf("logs = %+v, want messages %q", got, want)
		}
		for i := range want {
			if got[i].message != want[i] {
				t.Errorf("log[%d] = %q, want %q", i, got[i].message, want[i])
			}
			if strings.Contains(fmt.Sprint(got[i]), "server-controlled-value") {
				t.Errorf("log exposed server-controlled text: %+v", got[i])
			}
		}
		if got[0].attrs["status"] != int64(http.StatusServiceUnavailable) || got[0].attrs["retry_after_ms"] != int64(1000) || got[0].attrs["error_code"] != "http_error" {
			t.Fatalf("first failure attrs = %+v", got[0].attrs)
		}
	})
}

func TestSchedulerReportFailureLoggingHTTPStatusAndBackoff(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		retryAfter string
		wantDelay  int64
		wantEvent  string
	}{
		{name: "bad_request", status: http.StatusBadRequest, wantDelay: 1000, wantEvent: "report_delivery_failed"},
		{name: "unauthorized", status: http.StatusUnauthorized, wantDelay: 60000, wantEvent: "credentials_invalid"},
		{name: "rate_limited", status: http.StatusTooManyRequests, retryAfter: "17", wantDelay: 17000, wantEvent: "report_delivery_failed"},
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
				logs := &testLogSink{}
				scheduler := service.NewScheduler(service.SchedulerOptions{Logger: logs.logger(), Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, RandomFloat: func() float64 { return 0.5 },
					Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
					Report: func(context.Context, v1.Report) (v1.ReportResponse, error) {
						return v1.ReportResponse{}, &service.HTTPError{StatusCode: tt.status, RetryAfter: tt.retryAfter}
					}})
				done := make(chan error, 1)
				go func() { done <- scheduler.Run(ctx) }()
				synctest.Wait()
				records := logs.snapshot()
				if len(records) == 0 {
					t.Fatal("HTTP failure was not logged")
				}
				got := records[0]
				if got.message != tt.wantEvent || got.attrs["status"] != int64(tt.status) || got.attrs["retry_after_ms"] != tt.wantDelay || got.attrs["error_code"] != "http_error" {
					t.Fatalf("log = %+v", got)
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

func TestSchedulerUnauthorizedFailureAndRecoveryLogOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cfg := runtimeSettings(5)
		var attempts atomic.Int64
		logs := &testLogSink{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		scheduler := service.NewScheduler(service.SchedulerOptions{
			Logger: logs.logger(), Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, RandomFloat: func() float64 { return 0.5 },
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				if attempts.Add(1) == 1 {
					return v1.ReportResponse{}, &service.HTTPError{StatusCode: http.StatusUnauthorized}
				}
				return v1.ReportResponse{AcceptedSeq: report.Seq}, nil
			},
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		records := logs.snapshot()
		if len(records) != 2 {
			t.Fatalf("logs = %+v, want one failure and one recovery", records)
		}
		if records[0].message != "credentials_invalid" || records[0].attrs["error_code"] != "http_error" || records[0].attrs["status"] != int64(http.StatusUnauthorized) {
			t.Fatalf("failure log = %+v", records[0])
		}
		if records[1].message != "credentials_recovered" {
			t.Fatalf("recovery log = %+v", records[1])
		}
	})
}

func TestSchedulerReportFailureLoggingClassifiesLocalErrors(t *testing.T) {
	for _, tt := range []struct {
		name     string
		err      error
		wantCode string
	}{
		{name: "timeout", err: context.DeadlineExceeded, wantCode: "timeout"},
		{name: "connection", err: errors.New("secret URL and bearer value"), wantCode: "connection_failed"},
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
				logs := &testLogSink{}
				scheduler := service.NewScheduler(service.SchedulerOptions{Logger: logs.logger(), Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, RandomFloat: func() float64 { return 0.5 },
					Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
					Report:  func(context.Context, v1.Report) (v1.ReportResponse, error) { return v1.ReportResponse{}, tt.err }})
				done := make(chan error, 1)
				go func() { done <- scheduler.Run(ctx) }()
				synctest.Wait()
				records := logs.snapshot()
				if len(records) == 0 {
					t.Fatal("first failure was not logged")
				}
				got := records[0]
				if got.message != "report_delivery_failed" || got.attrs["error_code"] != tt.wantCode || strings.Contains(fmt.Sprint(got), "secret") {
					t.Fatalf("log = %+v", got)
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
		cfg := runtimeSettings(5)
		ctx, cancel := context.WithCancel(context.Background())
		logs := &testLogSink{}
		scheduler := service.NewScheduler(service.SchedulerOptions{Logger: logs.logger(), Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report: func(ctx context.Context, _ v1.Report) (v1.ReportResponse, error) {
				<-ctx.Done()
				return v1.ReportResponse{}, ctx.Err()
			}})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
		if got := logs.snapshot(); len(got) != 0 {
			t.Fatalf("cancellation emitted logs: %+v", got)
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
		cfg := runtimeSettings(5)
		client, err := service.NewClient(service.ClientOptions{ServerURL: "https://server-controlled.example",
			HTTPClient: &http.Client{Transport: loggingTimeoutTransport{}},
			Identity:   state})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logs := &testLogSink{}
		scheduler := service.NewScheduler(service.SchedulerOptions{Logger: logs.logger(), Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return cfg }, RandomFloat: func() float64 { return 0.5 },
			Collect: func(context.Context) (v1.Metrics, error) { return schedulerMetrics(), nil },
			Report:  client.Report})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		records := logs.snapshot()
		if len(records) == 0 {
			t.Fatal("client timeout was not logged")
		}
		got := records[0]
		if got.message != "report_delivery_failed" || got.attrs["error_code"] != "timeout" {
			t.Fatalf("log = %+v", got)
		}
		if rendered := fmt.Sprint(got); strings.Contains(rendered, "secret") || strings.Contains(rendered, "example") {
			t.Fatalf("log exposed transport or URL text: %+v", got)
		}
		cancel()
		synctest.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
