package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/config"
	oblogging "orbitbeacon/internal/logging"
	v1 "orbitbeacon/internal/protocol/v1"
)

type TrafficReporter interface {
	Entries() []v1.Traffic
	Acknowledge([]v1.Traffic)
}

// ProbeReporter 返回缓存的检查结果，不等待网络操作。
type ProbeReporter interface {
	Results([]string) []v1.ProbeResult
}

type SchedulerOptions struct {
	Commands *commandStore
	Upgrade  *upgrader
	// UpgradeConfirmTimeout defaults to upgradeConfirmTimeout.
	UpgradeConfirmTimeout time.Duration
	Logger                *slog.Logger
	Probes                ProbeReporter
	Traffic               TrafficReporter
	Identity              *identity.Store
	// RuntimeSettings 返回不可变快照；只有 ReloadConfig 可在采集循环中
	// 同步发布新的配置快照。
	RuntimeSettings func() config.RuntimeSettings
	Reload          <-chan struct{}
	// ConfigRevision 包含不会出现在上报中的本地配置变化。
	ConfigRevision func() uint64
	// ReloadConfig 在修改任何运行中的组件前完成完整校验。
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
	latestSettings     config.RuntimeSettings
	commandSettings    config.RuntimeSettings
	collectNow         chan struct{}
	stopping           atomic.Bool
}

func NewScheduler(options SchedulerOptions) *Scheduler {
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{options: options, collectNow: make(chan struct{}, 1)}
}

// Run 只运行一个采集循环和一个上报任务。不保留离线队列，
// 通过通知唤醒上报任务，使其读取最新快照。
func (s *Scheduler) Run(ctx context.Context) (runErr error) {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("scheduler already running")
	}
	defer s.running.Store(false)
	s.stopping.Store(false)
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
	s.commandSettings = s.options.RuntimeSettings()
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
		case <-s.collectNow:
			timer.Reset(0)
		case _, ok := <-reload:
			if !ok {
				reload = nil
				continue
			}
			if s.stopping.Load() {
				continue
			}
			before := s.options.RuntimeSettings()
			var revision uint64
			if s.options.ConfigRevision != nil {
				revision = s.options.ConfigRevision()
			}
			reloadErr := errors.New("configuration loader unavailable")
			if s.options.ReloadConfig != nil {
				s.mu.Lock()
				reloadErr = s.options.ReloadConfig(ctx)
				if reloadErr == nil {
					s.commandSettings = s.options.RuntimeSettings()
				}
				s.mu.Unlock()
			}
			if reloadErr != nil {
				s.options.Logger.LogAttrs(ctx, slog.LevelWarn, "configuration_reload_rejected", oblogging.ErrorAttrs(reloadErr, "configuration_reload_rejected")...)
				continue
			}
			if before.Equal(s.options.RuntimeSettings()) && (s.options.ConfigRevision == nil || revision == s.options.ConfigRevision()) {
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
			snapshot := v1.Report{ProtocolVersion: 1, RunGeneration: state.RunGeneration, SessionID: state.SessionID, Seq: seq, SampledAt: time.Now().UTC(), System: s.options.System, Metrics: metrics, Traffic: []v1.Traffic{}}
			s.mu.Lock()
			s.latest = snapshot
			s.latestSettings = settings
			s.mu.Unlock()
			select {
			case notify <- struct{}{}:
			default:
			}
			if !s.stopping.Load() {
				timer.Reset(max(0, time.Until(started.Add(time.Duration(settings.ReportIntervalSeconds)*time.Second))))
			}
		}
	}
}

func (s *Scheduler) reportLoop(ctx context.Context, notify <-chan struct{}) error {
	parent := ctx
	upgradeCtx, cancelUpgrade := context.WithCancel(ctx)
	var upgradeDone chan upgradeOutcome
	defer func() {
		cancelUpgrade()
		if upgradeDone != nil {
			<-upgradeDone
		}
	}()
	firstReport := true
	// A build that runs but cannot report never crashes, so it must exit by itself
	// for the rollback script to count the attempt.
	var unconfirmed <-chan time.Time
	if s.options.Commands != nil && s.options.Commands.upgradeUnconfirmed() {
		timeout := s.options.UpgradeConfirmTimeout
		if timeout <= 0 {
			timeout = upgradeConfirmTimeout
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		unconfirmed = timer.C
	}
	exitUnconfirmed := func() error {
		s.options.Logger.Warn("upgrade_unconfirmed")
		return &ExitError{Code: 75}
	}
	completeUpgrade := func(result upgradeOutcome) error {
		upgradeDone = nil
		if result.status == "succeeded" {
			return &ExitError{Code: 75}
		}
		if result.err != nil {
			s.options.Logger.LogAttrs(parent, slog.LevelWarn, "upgrade_failed", append(oblogging.ErrorAttrs(result.err, "upgrade_failed"), slog.String("code", result.code))...)
		}
		s.options.Commands.finishUpgrade(result)
		return nil
	}
	stopping := false
	var stopID string
	var stopSampleSeq uint64
	var stopReport *v1.Report
	var stopSettings config.RuntimeSettings
	canceled := func() error {
		if stopping && parent.Err() == nil {
			return &ExitError{Code: 0}
		}
		return nil
	}
	backoff := NewBackoff(s.options.RandomFloat)
	var sent uint64
	var consecutiveFailures uint64
	var lastFailureLog time.Time
	for {
		select {
		case <-ctx.Done():
			return canceled()
		case <-unconfirmed:
			return exitUnconfirmed()
		case result := <-upgradeDone:
			if err := completeUpgrade(result); err != nil {
				return err
			}
			continue
		case <-notify:
		}
		for {
			// 合并上次请求或退避期间积累的采集通知，只上报最新的不可变快照。
			select {
			case <-notify:
			default:
			}
			s.mu.Lock()
			report := s.latest
			settings := s.latestSettings
			s.mu.Unlock()
			if stopping && report.Seq > stopSampleSeq {
				if stopReport == nil {
					snapshot := report
					stopReport = &snapshot
					stopSettings = settings
				}
				// An in-flight collection can publish after stop starts. Retrying
				// one snapshot keeps a lost response from advancing last_seen.
				report = *stopReport
				settings = stopSettings
			}
			if report.Seq <= sent && !(stopping && s.options.Commands != nil && len(s.options.Commands.state.Pending) > 0) {
				break
			}
			if s.options.Traffic != nil && !settings.FeatureDisabled(v1.FeatureTraffic) {
				report.Traffic = s.options.Traffic.Entries()
			}
			if s.options.Probes != nil && !settings.FeatureDisabled(v1.FeatureProbes) {
				report.ProbeResults = s.options.Probes.Results(settings.ProbeTargets)
			}
			if s.options.Commands != nil {
				report.CommandResults = s.options.Commands.results()
			}
			requested := time.Now()
			response, err := s.options.Report(ctx, report)
			if ctx.Err() != nil {
				return canceled()
			}
			if err == nil {
				if firstReport && (s.options.Commands == nil || s.options.Commands.confirmUpgrade()) {
					firstReport = false
					unconfirmed = nil
				}
				if upgradeDone != nil {
					select {
					case result := <-upgradeDone:
						if err := completeUpgrade(result); err != nil {
							return err
						}
					default:
					}
				}
				if s.options.Commands != nil {
					s.options.Commands.acknowledge(report.CommandResults)
					if stopping {
						for _, result := range report.CommandResults {
							if result.ID == stopID {
								return &ExitError{Code: 0}
							}
						}
					}
					if !stopping && response.Command != nil && !(response.Command.Kind == v1.RemoteCommandUpgrade && upgradeDone != nil) {
						s.mu.Lock()
						// 服务器时间生成于响应之前，计入请求耗时才不会执行已过期的指令。
						serverTime := response.ServerTime
						if !serverTime.IsZero() {
							serverTime = serverTime.Add(time.Since(requested))
						}
						upgradeDeadline := time.Now().Add(min(upgradeDownloadTimeout, response.Command.ExpiresAt.Sub(serverTime)-upgradeReserve))
						action := s.options.Commands.handle(*response.Command, serverTime, s.commandSettings)
						s.mu.Unlock()
						if action == v1.RemoteCommandUpgrade {
							if s.options.Upgrade == nil {
								s.options.Commands.finishUpgrade(upgradeOutcome{id: strings.ToLower(response.Command.ID), status: "rejected", code: "unsupported"})
							} else {
								command := *response.Command
								done := make(chan upgradeOutcome, 1)
								upgradeDone = done
								go func() {
									downloadCtx, cancel := context.WithDeadline(upgradeCtx, upgradeDeadline)
									defer cancel()
									done <- s.options.Upgrade.run(downloadCtx, command)
								}()
							}
						}
						if action == v1.RemoteCommandRestart {
							return &ExitError{Code: 75}
						}
						if action == v1.RemoteCommandStop {
							stopping = true
							stopID = strings.ToLower(response.Command.ID)
							stopSampleSeq = report.Seq
							s.stopping.Store(true)
							stopCtx, stopCancel := context.WithTimeout(parent, 15*time.Second)
							defer stopCancel()
							ctx = stopCtx
							select {
							case s.collectNow <- struct{}{}:
							default:
							}
						}
					}
				}
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
				if stopping && report.Seq > stopSampleSeq && len(s.options.Commands.state.Pending) > 0 {
					continue
				}
				break
			}
			status, retryAfter := 0, ""
			var failure *HTTPError
			if errors.As(err, &failure) {
				status, retryAfter = failure.StatusCode, failure.RetryAfter
				if status == http.StatusConflict && failure.Code == "identity_conflict" && !stopping {
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
				return canceled()
			case <-unconfirmed:
				timer.Stop()
				return exitUnconfirmed()
			case result := <-upgradeDone:
				timer.Stop()
				if err := completeUpgrade(result); err != nil {
					return err
				}
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
