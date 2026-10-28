package probe

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func successful(target string) v1.ProbeResult {
	latency := 1.25
	return v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, CheckedAt: time.Now().UTC()}
}

func TestRunnerIntervalExplicitSelectionAndLatest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var checks atomic.Int64
		runner := NewRunner(Options{
			Check: func(_ context.Context, target string) v1.ProbeResult { checks.Add(1); return successful(target) },
		})
		if err := runner.SetTargets([]string{"tcp://first:80"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		first := runner.Results([]string{"tcp://first:80"})
		if len(first) != 1 || checks.Load() != 1 {
			t.Fatalf("initial check not prompt: %v, %d", first, checks.Load())
		}
		*first[0].LatencyMS = 999
		latest := runner.Results([]string{"tcp://first:80"})
		if *latest[0].LatencyMS != 1.25 {
			t.Fatal("caller mutated cached result")
		}
		time.Sleep(59 * time.Second)
		synctest.Wait()
		latest = runner.Results([]string{"tcp://first:80"})
		if checks.Load() != 1 || !latest[0].CheckedAt.Equal(first[0].CheckedAt) {
			t.Fatal("result did not repeat unchanged before 60s")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		latest = runner.Results([]string{"tcp://first:80"})
		if checks.Load() != 2 || !latest[0].CheckedAt.After(first[0].CheckedAt) {
			t.Fatal("60s check did not run")
		}
		if err := runner.SetTargets([]string{"tcp://second:443"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		latest = runner.Results([]string{"tcp://second:443"})
		if checks.Load() != 3 || len(latest) != 1 || latest[0].Target != "tcp://second:443" {
			t.Fatalf("config update not prompt: %+v", latest)
		}
		if err := runner.SetTargets(nil); err != nil {
			t.Fatal(err)
		}
		// Reading an older report's target set must not resurrect removed work.
		runner.Results([]string{"tcp://second:443"})
		synctest.Wait()
		if latest := runner.Results(nil); len(latest) != 0 {
			t.Fatalf("removed results: %+v", latest)
		}
		if checks.Load() != 3 {
			t.Fatal("reading results re-added a removed target")
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestRunnerBoundedConcurrencyAndNonblockingSnapshots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var active, peak, calls atomic.Int64
		runner := NewRunner(Options{Interval: time.Second, Check: func(ctx context.Context, target string) v1.ProbeResult {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			calls.Add(1)
			<-ctx.Done()
			// Even a check that returns success on cancellation must not resurrect a
			// removed target or populate its newly added incarnation.
			return successful(target)
		}})
		targets := []string{}
		for i := range 5 {
			targets = append(targets, fmt.Sprintf("tcp://host%d:80", i))
		}
		if err := runner.SetTargets(targets); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		if calls.Load() != 5 || active.Load() != 5 {
			t.Fatalf("checks not concurrent: calls=%d active=%d", calls.Load(), active.Load())
		}
		if got := runner.Results(targets); len(got) != 0 {
			t.Fatal("unfinished checks included")
		}
		if err := runner.SetTargets(nil); err != nil {
			t.Fatal(err)
		}
		if err := runner.SetTargets(targets); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := runner.Results(targets); len(got) != 0 {
			t.Fatalf("old completion published after remove/readd: %+v", got)
		}
		if calls.Load() != 10 || active.Load() != 5 || peak.Load() != 5 {
			t.Fatalf("concurrency after churn: calls=%d active=%d peak=%d", calls.Load(), active.Load(), peak.Load())
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if active.Load() != 0 {
			t.Fatal("check workers leaked")
		}
	})
}

func TestRunnerInjectableIntervalAndCheckDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var checks atomic.Int64
		runner := NewRunner(Options{Interval: 10 * time.Second, Timeout: time.Second, Check: func(ctx context.Context, target string) v1.ProbeResult {
			checks.Add(1)
			<-ctx.Done()
			return v1.ProbeResult{Target: target, Error: "timeout", CheckedAt: time.Now().UTC()}
		}})
		targets := []string{"https://example.com/"}
		if err := runner.SetTargets(targets); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		result := runner.Results(targets)
		if len(result) != 1 || result[0].Error != "timeout" {
			t.Fatalf("deadline result: %+v", result)
		}
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if checks.Load() != 2 {
			t.Fatal("injected interval ignored")
		}
		cancel()
		synctest.Wait()
		<-done
	})
}

func TestRunnerRejectsInvalidSelectionWithoutChangingTargets(t *testing.T) {
	r := NewRunner(Options{})
	if err := r.SetTargets([]string{"tcp://host:80"}); err != nil {
		t.Fatal(err)
	}
	for _, targets := range [][]string{{"bad"}, {"tcp://host:80", "tcp://host:80"}, {"tcp://a:1", "tcp://b:1", "tcp://c:1", "tcp://d:1", "tcp://e:1", "tcp://f:1"}} {
		if err := r.SetTargets(targets); err == nil {
			t.Fatalf("accepted %v", targets)
		}
	}
	if len(r.targets) != 1 || r.targets["tcp://host:80"] == nil {
		t.Fatal("invalid selection changed targets")
	}
}
