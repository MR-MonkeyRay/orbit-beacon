package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/agent/identity"
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
	Probes      ProbeReporter
	Traffic     TrafficReporter
	Identity    *identity.Store
	Config      *ConfigStore
	Collect     func(context.Context) (v1.Metrics, error)
	Report      func(context.Context, v1.Report) (v1.ReportResponse, error)
	RandomFloat func() float64
	System      *v1.System
	// CollectedRevision identifies settings actually used by the collector when
	// a remote configuration arrives during a sample. Nil uses Config.Current.
	CollectedRevision  func() int64
	CredentialsChanged func(bool)
	Log                func(string)
}

type Scheduler struct {
	options            SchedulerOptions
	running            atomic.Bool
	credentialsInvalid atomic.Bool
	mu                 sync.Mutex
	latest             v1.Report
}

func NewScheduler(options SchedulerOptions) *Scheduler { return &Scheduler{options: options} }

func (s *Scheduler) CredentialsInvalid() bool { return s.credentialsInvalid.Load() }

func (s *Scheduler) setCredentialsInvalid(invalid bool) {
	if s.credentialsInvalid.Swap(invalid) != invalid && s.options.CredentialsChanged != nil {
		s.options.CredentialsChanged(invalid)
	}
}

// Run owns exactly one collection loop and one reporting worker. No offline
// queue is retained: the notification channel signals a single latest snapshot.
func (s *Scheduler) Run(ctx context.Context) (runErr error) {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("scheduler already running")
	}
	defer s.running.Store(false)
	if s.options.Identity == nil || s.options.Config == nil || s.options.Collect == nil || s.options.Report == nil {
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
	capabilities := &v1.Capabilities{SelfUpdate: false}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			started := time.Now()
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
			revision, interval := s.options.Config.settings()
			if s.options.CollectedRevision != nil {
				revision = s.options.CollectedRevision()
			}
			snapshot := v1.Report{ProtocolVersion: 1, RunGeneration: state.RunGeneration, SessionID: state.SessionID, Seq: seq, SampledAt: time.Now().UTC(), ConfigRevision: revision, System: s.options.System, Metrics: metrics, Traffic: []v1.Traffic{}, Capabilities: capabilities, ConfigError: s.options.Config.ConfigError()}
			s.mu.Lock()
			s.latest = snapshot
			s.mu.Unlock()
			select {
			case notify <- struct{}{}:
			default:
			}
			timer.Reset(max(0, time.Until(started.Add(time.Duration(interval)*time.Second))))
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
				report.ProbeResults = s.options.Probes.Results(s.options.Config.Current().ProbeTargets)
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
				s.setCredentialsInvalid(false)
				if consecutiveFailures > 0 && s.options.Log != nil {
					s.options.Log(fmt.Sprintf("report recovered after %d consecutive failures", consecutiveFailures))
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
			if status == 401 {
				s.setCredentialsInvalid(true)
			}
			now := time.Now()
			delay := backoff.Next(status, retryAfter, now)
			consecutiveFailures++
			if s.options.Log != nil && (consecutiveFailures == 1 || now.Sub(lastFailureLog) >= 5*time.Minute) {
				state := fmt.Sprintf("error=%s", reportFailureKind(err))
				if status != 0 {
					state = fmt.Sprintf("status=%d", status)
				}
				if consecutiveFailures == 1 {
					s.options.Log(fmt.Sprintf("report failed: %s; retrying in %s", state, delay))
				} else {
					s.options.Log(fmt.Sprintf("report still failing: consecutive_failures=%d; %s; retrying in %s", consecutiveFailures, state, delay))
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
