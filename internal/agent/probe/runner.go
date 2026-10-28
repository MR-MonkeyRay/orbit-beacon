package probe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

const (
	DefaultInterval = 60 * time.Second
	DefaultTimeout  = 5 * time.Second
	maxConcurrent   = 5
)

// Options permits short intervals and synthetic checks in tests. Targets must
// return the currently applied configuration; it is polled once per second.
type Options struct {
	Targets        func() []string
	Check          func(context.Context, string) v1.ProbeResult
	Interval       time.Duration
	Timeout        time.Duration
	ConfigInterval time.Duration
}

type targetState struct {
	next    time.Time
	running bool
	cancel  context.CancelFunc
	result  *v1.ProbeResult
}

// Runner keeps one latest result per target. Its reporting methods never wait
// for network work; all checks share a five-worker concurrency limit.
type Runner struct {
	options Options
	mu      sync.Mutex
	targets map[string]*targetState
	active  int
	wake    chan struct{}
	running atomic.Bool
}

func NewRunner(options Options) *Runner {
	if options.Interval <= 0 {
		options.Interval = DefaultInterval
	}
	if options.Timeout <= 0 {
		options.Timeout = DefaultTimeout
	}
	if options.ConfigInterval <= 0 {
		options.ConfigInterval = time.Second
	}
	return &Runner{options: options, targets: make(map[string]*targetState), wake: make(chan struct{}, 1)}
}

// SetTargets immediately removes stale results and cancels removed checks.
// Adding a target wakes the runner without waiting for its next interval.
func (r *Runner) SetTargets(targets []string) error {
	if len(targets) > maxConcurrent {
		return errors.New("too many probe targets")
	}
	desired := make(map[string]bool, len(targets))
	for _, target := range targets {
		if err := v1.ValidateProbeTarget(target); err != nil {
			return err
		}
		if desired[target] {
			return errors.New("duplicate probe target")
		}
		desired[target] = true
	}
	r.mu.Lock()
	changed := false
	for target, state := range r.targets {
		if !desired[target] {
			if state.cancel != nil {
				state.cancel()
			}
			delete(r.targets, target)
			changed = true
		}
	}
	for target := range desired {
		if r.targets[target] == nil {
			r.targets[target] = &targetState{}
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.signal()
	}
	return nil
}

// Results returns independent copies in configuration order, omitting checks
// that have not completed. Applying the selection here also prevents a removed
// target leaking into a report between configuration polls.
func (r *Runner) Results(targets []string) []v1.ProbeResult {
	if r.SetTargets(targets) != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var results []v1.ProbeResult
	for _, target := range targets {
		state := r.targets[target]
		if state == nil || state.result == nil {
			continue
		}
		result := *state.result
		if result.LatencyMS != nil {
			value := *result.LatencyMS
			result.LatencyMS = &value
		}
		if result.StatusCode != nil {
			value := *result.StatusCode
			result.StatusCode = &value
		}
		results = append(results, result)
	}
	return results
}

func (r *Runner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) Run(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return errors.New("probe runner already running")
	}
	defer r.running.Store(false)
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	check := r.options.Check
	if check == nil {
		checker := NewChecker(r.options.Timeout)
		defer checker.Close()
		check = checker.Check
	}
	defer func() { cancel(); workers.Wait() }()
	poll := func() {
		if r.options.Targets != nil {
			_ = r.SetTargets(r.options.Targets())
		}
	}
	poll()
	configTicker := time.NewTicker(r.options.ConfigInterval)
	defer configTicker.Stop()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now := time.Now()
		r.mu.Lock()
		for target, state := range r.targets {
			if r.active == maxConcurrent {
				break
			}
			if state.running || state.next.After(now) {
				continue
			}
			checkCtx, stop := context.WithTimeout(ctx, r.options.Timeout)
			state.running, state.cancel, state.next = true, stop, now.Add(r.options.Interval)
			r.active++
			workers.Add(1)
			go func(target string, state *targetState) {
				defer workers.Done()
				result := check(checkCtx, target)
				stop()
				r.mu.Lock()
				r.active--
				state.running, state.cancel = false, nil
				if r.targets[target] == state && ctx.Err() == nil {
					state.result = &result
				}
				r.mu.Unlock()
				r.signal()
			}(target, state)
		}
		var next time.Time
		if r.active < maxConcurrent {
			for _, state := range r.targets {
				if !state.running && (next.IsZero() || state.next.Before(next)) {
					next = state.next
				}
			}
		}
		r.mu.Unlock()
		timer.Stop()
		var due <-chan time.Time
		if !next.IsZero() {
			timer.Reset(max(0, time.Until(next)))
			due = timer.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-configTicker.C:
			poll()
		case <-due:
		case <-r.wake:
		}
	}
}
