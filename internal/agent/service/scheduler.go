package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/agent/identity"
	oblogging "orbitbeacon/internal/logging"
	v1 "orbitbeacon/internal/protocol/v1"
)

type TrafficReporter interface {
	Entries() []v1.Traffic
	Acknowledge([]v1.Traffic)
}

// ProbeReporter returns cached checks without waiting for network work.
type ProbeReporter interface {
	Results([]string) []v1.ProbeResult
}

type SchedulerOptions struct {
	Logger   *slog.Logger
	Probes   ProbeReporter
	Traffic  TrafficReporter
	Identity *identity.Store
	// RuntimeSettings returns an immutable snapshot; only ReloadConfig may
	// publish a replacement, synchronously on this collection loop.
	RuntimeSettings func() v1.RuntimeSettings
	Reload          <-chan struct{}
	// ReloadConfig validates completely before changing any active component.
	ReloadConfig func(context.Context) error
	Collect      func(context.Context) (v1.Metrics, error)
	Report       func(context.Context, v1.Report) (v1.ReportResponse, error)
	RandomFloat  func() float64
	System       *v1.System
}

type Scheduler struct {
	options            SchedulerOptions
	running            atomic.Bool
	credentialsInvalid atomic.Bool
	mu                 sync.Mutex
	latest             v1.Report
}

func NewScheduler(options SchedulerOptions) *Scheduler {
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{options: options}
}

// Run owns exactly one collection loop and one reporting worker. No offline
// queue is retained: the notification channel signals a single latest snapshot.
func (s *Scheduler) Run(ctx context.Context) (runErr error) {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("scheduler already running")
	}
	defer s.running.Store(false)
	if s.options.Identity == nil || s.options.RuntimeSettings == nil || s.options.Collect == nil || s.options.Report == nil {
		return errors.New("scheduler dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := s.options.Identity.BeginRun(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	notify := make(chan struct{}, 1)
	done := make(chan struct{})
	var reportErr error
	go func() {
		defer close(done)
		reportErr = s.reportLoop(ctx, notify)
		cancel()
	}()
	defer func() {
		cancel()
		<-done
		if reportErr != nil {
			runErr = reportErr
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	var seq uint64
	reload := s.options.Reload
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-reload:
			if !ok {
				reload = nil
				continue
			}
			before := s.options.RuntimeSettings()
			reloadErr := errors.New("configuration loader unavailable")
			if s.options.ReloadConfig != nil {
				reloadErr = s.options.ReloadConfig(ctx)
			}
			if reloadErr != nil {
				s.options.Logger.LogAttrs(ctx, slog.LevelWarn, "configuration_reload_rejected", oblogging.ErrorAttrs(reloadErr, "configuration_reload_rejected")...)
				continue
			}
			if sameRuntimeSettings(before, s.options.RuntimeSettings()) {
				s.options.Logger.InfoContext(ctx, "configuration_unchanged")
				continue
			}
			s.options.Logger.InfoContext(ctx, "configuration_reloaded")
			timer.Reset(0)
		case <-timer.C:
			started := time.Now()
			settings := s.options.RuntimeSettings()
			collectionCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			metrics, collectErr := s.options.Collect(collectionCtx)
			stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if collectErr != nil {
				if metrics.MetricStatus == nil {
					metrics.MetricStatus = make(map[string]string, len(v1.MetricStatusFields()))
				}
				for _, field := range v1.MetricStatusFields() {
					if metrics.MetricStatus[field] == "" {
						metrics.MetricStatus[field] = "error"
					}
				}
			}
			seq++
			snapshot := v1.Report{ProtocolVersion: 1, RunGeneration: state.RunGeneration, SessionID: state.SessionID, Seq: seq, SampledAt: time.Now().UTC(), Runtime: settings, System: s.options.System, Metrics: metrics, Traffic: []v1.Traffic{}}
			s.mu.Lock()
			s.latest = snapshot
			s.mu.Unlock()
			select {
			case notify <- struct{}{}:
			default:
			}
			timer.Reset(max(0, time.Until(started.Add(time.Duration(settings.ReportIntervalSeconds)*time.Second))))
		}
	}
}

func (s *Scheduler) reportLoop(ctx context.Context, notify <-chan struct{}) error {
	backoff := NewBackoff(s.options.RandomFloat)
	var sent uint64
	var consecutiveFailures uint64
	var lastFailureLog time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-notify:
		}
		for {
			// Collapse all collection notifications received during the last request
			// or backoff into the latest immutable snapshot.
			select {
			case <-notify:
			default:
			}
			s.mu.Lock()
			report := s.latest
			s.mu.Unlock()
			if report.Seq <= sent {
				break
			}
			if s.options.Traffic != nil {
				report.Traffic = s.options.Traffic.Entries()
			}
			if s.options.Probes != nil {
				report.ProbeResults = s.options.Probes.Results(report.Runtime.ProbeTargets)
			}
			response, err := s.options.Report(ctx, report)
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				if s.options.Traffic != nil {
					s.options.Traffic.Acknowledge(response.TrafficAcks)
				}
				sent = report.Seq
				backoff.Reset()
				credentialsRecovered := s.credentialsInvalid.Swap(false)
				if consecutiveFailures > 0 {
					event := "report_delivery_recovered"
					if credentialsRecovered {
						event = "credentials_recovered"
					}
					s.options.Logger.InfoContext(ctx, event, "consecutive_failures", consecutiveFailures)
				}
				consecutiveFailures = 0
				lastFailureLog = time.Time{}
				break
			}
			status, retryAfter := 0, ""
			var failure *HTTPError
			if errors.As(err, &failure) {
				status, retryAfter = failure.StatusCode, failure.RetryAfter
				if status == http.StatusConflict && failure.Code == "identity_conflict" {
					return err
				}
			}
			credentialsBecameInvalid := false
			if status == http.StatusUnauthorized {
				credentialsBecameInvalid = !s.credentialsInvalid.Swap(true)
			}
			now := time.Now()
			delay := backoff.Next(status, retryAfter, now)
			consecutiveFailures++
			if consecutiveFailures == 1 || credentialsBecameInvalid || now.Sub(lastFailureLog) >= 5*time.Minute {
				errorCode := reportFailureKind(err)
				if status != 0 {
					errorCode = "http_error"
				}
				attrs := oblogging.ErrorAttrs(err, errorCode)
				if status != 0 {
					attrs = append(attrs, slog.Int("status", status))
				}
				attrs = append(attrs, slog.Uint64("consecutive_failures", consecutiveFailures), slog.Int64("retry_after_ms", delay.Milliseconds()))
				if credentialsBecameInvalid {
					s.options.Logger.LogAttrs(ctx, slog.LevelWarn, "credentials_invalid", attrs...)
				} else if consecutiveFailures == 1 {
					s.options.Logger.LogAttrs(ctx, slog.LevelWarn, "report_delivery_failed", attrs...)
				} else {
					s.options.Logger.LogAttrs(ctx, slog.LevelWarn, "report_delivery_still_failing", attrs...)
				}
				lastFailureLog = now
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	}
}

func reportFailureKind(err error) string {
	if errors.Is(err, errAgentTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	return "connection_failed"
}
