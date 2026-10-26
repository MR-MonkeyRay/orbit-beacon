package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/agent/identity"
	v1 "orbitbeacon/internal/protocol/v1"
)

type SchedulerOptions struct {
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
			_, err := s.options.Report(ctx, report)
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				sent = report.Seq
				backoff.Reset()
				s.setCredentialsInvalid(false)
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
			timer := time.NewTimer(backoff.Next(status, retryAfter, time.Now()))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	}
}
