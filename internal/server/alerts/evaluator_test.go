package alerts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func ptr[T any](v T) *T { return &v }

type latestStub struct {
	report ingest.LatestReport
	err    error
}

func (s *latestStub) Latest(context.Context, string) (ingest.LatestReport, error) {
	return s.report, s.err
}

type alertFixture struct {
	db     *sql.DB
	s      *Service
	latest *latestStub
	e      *Evaluator
	now    time.Time
	node   string
	rule   Rule
}

func patch(t *testing.T, raw string) Patch {
	t.Helper()
	var p Patch
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func newFixture(t *testing.T, seconds int) *alertFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &alertFixture{db: db, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), node: "00000000-0000-4000-8000-000000000001", latest: &latestStub{}}
	f.s = NewService(db, func() time.Time { return f.now })
	if _, err = db.Exec("INSERT INTO nodes(id,name,status,token_hash,first_enrolled_at,last_seen,created_at) VALUES(?,'node','unknown',x'01',?,?,?)", f.node, timekey.Format(f.now), timekey.Format(f.now), timekey.Format(f.now)); err != nil {
		t.Fatal(err)
	}
	p := patch(t, `{"name":"cpu hot","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning"}`)
	p.ForSeconds, _ = json.Marshal(seconds)
	f.rule, err = f.s.Save(context.Background(), "", p)
	if err != nil {
		t.Fatal(err)
	}
	f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{})
	f.value(90)
	return f
}
func (f *alertFixture) value(v float64) {
	f.latest.err = nil
	f.latest.report = ingest.LatestReport{ReceivedAt: f.now, Report: v1.Report{Runtime: v1.DefaultRuntimeSettings(), Metrics: v1.Metrics{CPUPercent: ptr(v), MetricStatus: map[string]string{"cpu_percent": "ok"}, Sources: map[string]v1.SourceMetadata{"cpu": {SampleID: "cpu", SourceIntervalMS: 10000}}}}}
}
func (f *alertFixture) tick(t *testing.T) {
	t.Helper()
	if err := f.e.Evaluate(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
}
func (f *alertFixture) advance(seconds int) { f.now = f.now.Add(time.Duration(seconds) * time.Second) }
func (f *alertFixture) events(t *testing.T, state string) []Event {
	t.Helper()
	p, err := f.s.Events(context.Background(), state, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	return p.Events
}
func (f *alertFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestPendingFiringHysteresisRecoveryAndRestart(t *testing.T) {
	f := newFixture(t, 60)
	f.tick(t)
	if len(f.e.pending) != 1 || len(f.events(t, "all")) != 0 {
		t.Fatal("expected pending without event")
	}
	f.advance(59)
	f.value(90)
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("fired early")
	}
	f.advance(1)
	f.value(90)
	f.tick(t)
	events := f.events(t, "firing")
	if len(events) != 1 {
		t.Fatalf("events=%+v", events)
	}
	id := events[0].ID
	if !events[0].StartedAt.Equal(f.now.Add(-time.Minute)) {
		t.Fatal("started_at should preserve pending start")
	}
	f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{})
	f.tick(t)
	if len(f.events(t, "all")) != 1 {
		t.Fatal("restart duplicated open event")
	}
	f.advance(1)
	f.value(79)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("hysteresis resolved too early")
	}
	f.advance(1)
	f.value(78)
	f.tick(t)
	events = f.events(t, "resolved")
	if len(events) != 1 || events[0].ID != id || events[0].ResolvedAt == nil || *events[0].Value != 78 {
		t.Fatalf("recovery=%+v", events)
	}
	f.advance(1)
	f.value(90)
	f.tick(t)
	if len(f.events(t, "all")) != 1 {
		t.Fatal("new episode should be pending")
	}
}
func TestNoDataBreaksPendingAndNeverFiresOrEstablishesRecovery(t *testing.T) {
	f := newFixture(t, 30)
	f.latest.err = nodes.ErrNotFound
	f.tick(t)
	if len(f.events(t, "all")) != 0 || len(f.e.pending) != 0 {
		t.Fatal("no data fired")
	}
	f.value(90)
	f.tick(t)
	f.advance(20)
	f.latest.err = nodes.ErrNotFound
	f.tick(t)
	f.advance(20)
	f.value(90)
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("no data did not reset pending")
	}
	f.advance(30)
	f.value(90)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("expected firing")
	}
	f.latest.err = nodes.ErrNotFound
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("missing data falsely resolved")
	}
}
func TestStaleAndFailedMetricsNeverFire(t *testing.T) {
	for _, mode := range []string{"old-report", "future-report", "old-source", "elapsed-source", "reported-stale", "failed", "missing-source", "missing-value"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, 0)
			switch mode {
			case "old-report":
				f.latest.report.ReceivedAt = f.now.Add(-nodes.OnlineWindow - time.Second)
			case "future-report":
				f.latest.report.ReceivedAt = f.now.Add(time.Second)
			case "old-source":
				s := f.latest.report.Metrics.Sources["cpu"]
				s.AgeMS = 46000
				f.latest.report.Metrics.Sources["cpu"] = s
			case "elapsed-source":
				f.now = f.now.Add(45*time.Second + time.Millisecond)
			case "reported-stale":
				f.latest.report.Metrics.MetricStatus["cpu_percent"] = "stale"
			case "failed":
				f.latest.report.Metrics.MetricStatus["cpu_percent"] = "error"
			case "missing-source":
				f.latest.report.Metrics.Sources = nil
			case "missing-value":
				f.latest.report.Metrics.CPUPercent = nil
			}
			f.tick(t)
			if len(f.events(t, "all")) != 0 {
				t.Fatal("invalid/stale data fired")
			}
		})
	}
}
func TestOfflineUsesLastSeenAndFreshMemoryPresenceWithoutDoubleDelay(t *testing.T) {
	f := newFixture(t, 0)
	r, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"offline","threshold":null,"comparator":null,"for_seconds":180}`))
	if err != nil {
		t.Fatal(err)
	}
	f.rule = r
	f.latest.err = nodes.ErrNotFound
	f.advance(179)
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("offline fired early")
	}
	f.advance(1)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("offline waited a second duration")
	}
	f.value(10)
	f.tick(t)
	if len(f.events(t, "resolved")) != 1 {
		t.Fatal("fresh in-memory report did not resolve offline")
	}
	f.advance(179)
	f.value(10)
	f.tick(t)
	if len(f.events(t, "all")) != 1 {
		t.Fatal("persisted last_seen overrode fresh in-memory presence")
	}
}
func TestDisableDeletionRevocationAndScopeChangesResolve(t *testing.T) {
	for _, mode := range []string{"disable-node", "delete-node", "disable-rule", "delete-rule", "revoke-node", "scope-change", "edit-condition"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, 0)
			f.tick(t)
			switch mode {
			case "disable-node":
				f.exec(t, "UPDATE nodes SET status='disabled' WHERE id=?", f.node)
			case "delete-node":
				f.exec(t, "DELETE FROM nodes WHERE id=?", f.node)
			case "revoke-node":
				f.exec(t, "UPDATE nodes SET token_hash=NULL WHERE id=?", f.node)
			case "disable-rule":
				_, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"enabled":false}`))
				if err != nil {
					t.Fatal(err)
				}
			case "delete-rule":
				if err := f.s.Delete(context.Background(), f.rule.ID); err != nil {
					t.Fatal(err)
				}
			case "scope-change":
				group := "00000000-0000-4000-8000-000000000002"
				f.exec(t, "INSERT INTO groups(id,name) VALUES(?,'group')", group)
				_, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"scope_kind":"group","scope_id":"`+group+`"}`))
				if err != nil {
					t.Fatal(err)
				}
			case "edit-condition":
				_, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"threshold":95}`))
				if err != nil {
					t.Fatal(err)
				}
			}
			// Reloading includes deleted resource IDs and snapshots.
			f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{})
			f.tick(t)
			events := f.events(t, "resolved")
			if len(events) != 1 || events[0].RuleName == "" || events[0].NodeName == "" {
				t.Fatalf("did not resolve with retained names: %+v", events)
			}
			if len(f.e.pending) != 0 {
				t.Fatal("orphaned pending")
			}
		})
	}
}
func TestPendingMemoryPrunedAfterNodeAndRuleDelete(t *testing.T) {
	f := newFixture(t, 60)
	f.tick(t)
	f.exec(t, "DELETE FROM nodes")
	f.tick(t)
	if len(f.e.pending) != 0 {
		t.Fatal("deleted nodes retained pending")
	}
}
func TestMetricComputationAndLessThanHysteresis(t *testing.T) {
	f := newFixture(t, 0)
	m := v1.Metrics{MemoryTotalBytes: ptr(uint64(100)), MemoryAvailableBytes: ptr(uint64(20)), SwapTotalBytes: ptr(uint64(100)), SwapUsedBytes: ptr(uint64(30)), Load1: ptr(2.5), Disks: []v1.DiskMetrics{{Path: "/", TotalBytes: ptr(uint64(1000)), UsedBytes: ptr(uint64(100))}, {Path: "/data", TotalBytes: ptr(uint64(100)), UsedBytes: ptr(uint64(90))}}, MetricStatus: map[string]string{}, Sources: map[string]v1.SourceMetadata{}}
	for key, source := range map[string]string{"memory_total_bytes": "memory", "memory_available_bytes": "memory", "swap_total_bytes": "swap", "swap_used_bytes": "swap", "load1": "load", "disk:/": "disk:/", "disk:/data": "disk:/data"} {
		m.MetricStatus[key] = "ok"
		m.Sources[source] = v1.SourceMetadata{SampleID: "sample", SourceIntervalMS: 10000}
	}
	f.latest.report.Metrics = m
	for metric, want := range map[string]float64{"memory_used_percent": 80, "swap_used_percent": 30, "load1": 2.5, "disk_used_percent": 90} {
		v := metricValue(f.latest.report, metric, f.now)
		if v == nil || *v != want {
			t.Errorf("%s=%v want %v", metric, v, want)
		}
	}
	r := Rule{Metric: "cpu_percent", Comparator: ptr("<"), Threshold: ptr(20.0)}
	if !condition(r, 19, false) || condition(r, 20, false) || !condition(r, 20.5, true) || condition(r, 21, true) {
		t.Fatal("less-than hysteresis")
	}
}

func TestProbesDownMetricCountsFailuresAndEmptyIsZero(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	latest := ingest.LatestReport{
		ReceivedAt: now,
		Report: v1.Report{Runtime: v1.RuntimeSettings{ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, Interfaces: []string{}, DiskPaths: []string{}, ProbeTargets: []string{"tcp://one.example:443", "https://two.example/health", "icmp://three.example"}}, ProbeResults: []v1.ProbeResult{
			{Target: "tcp://one.example:443", OK: true},
			{Target: "https://two.example/health", OK: false},
			{Target: "icmp://three.example", OK: false},
		}},
	}
	value := metricValue(latest, "probes_down", now)
	if value == nil || *value != 2 {
		t.Fatalf("probes_down=%v want 2", value)
	}
	latest.ProbeResults = nil
	value = metricValue(latest, "probes_down", now)
	if value == nil || *value != 0 {
		t.Fatalf("empty probes_down=%v want 0", value)
	}
}

func TestProbesDownHysteresisUsesCountBounds(t *testing.T) {
	greater := Rule{Metric: "probes_down", Comparator: ptr(">"), Threshold: ptr(0.0)}
	if condition(greater, 0, true) || !condition(greater, 1, false) {
		t.Fatal(">0 probes_down did not resolve at zero or fire above zero")
	}
	less := Rule{Metric: "probes_down", Comparator: ptr("<"), Threshold: ptr(5.0)}
	if condition(less, 5, true) || !condition(less, 4, false) {
		t.Fatal("<5 probes_down did not recover at the upper count bound")
	}
}

func TestProbesDownFiresAndEmptyResultsResolve(t *testing.T) {
	f := newFixture(t, 0)
	r, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"metric":"probes_down","threshold":0}`))
	if err != nil {
		t.Fatal(err)
	}
	f.rule = r
	f.latest.report = ingest.LatestReport{
		ReceivedAt: f.now,
		Report: v1.Report{Runtime: v1.RuntimeSettings{ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, Interfaces: []string{}, DiskPaths: []string{}, ProbeTargets: []string{"tcp://one.example:443", "https://two.example/health"}}, ProbeResults: []v1.ProbeResult{
			{Target: "tcp://one.example:443", OK: true},
			{Target: "https://two.example/health", OK: false},
		}},
	}
	f.tick(t)
	events := f.events(t, "firing")
	if len(events) != 1 || events[0].Value == nil || *events[0].Value != 1 {
		t.Fatalf("firing events=%+v", events)
	}
	f.latest.report.ProbeResults = nil
	f.tick(t)
	events = f.events(t, "resolved")
	if len(events) != 1 || events[0].Value == nil || *events[0].Value != 0 {
		t.Fatalf("resolved events=%+v", events)
	}
}

type blockingNotifier struct{ entered chan Notification }

func (n blockingNotifier) Notify(ctx context.Context, v Notification) { n.entered <- v; <-ctx.Done() }
func TestNotifierIsBoundedAfterCommitAndStopsWithContext(t *testing.T) {
	f := newFixture(t, 0)
	entered := make(chan Notification, 1)
	notifier := blockingNotifier{entered: entered}
	f.e = NewEvaluator(f.s, f.latest, EvaluatorOptions{Notifier: notifier, Interval: time.Millisecond, QueueSize: 1, Now: func() time.Time { return f.now }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.e.Run(ctx) }()
	select {
	case n := <-entered:
		if n.State != "firing" || len(f.events(t, "firing")) != 1 {
			t.Fatal("notify before commit")
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no notification")
	}
	// A blocked notifier cannot hold the evaluator. Its queue stays bounded.
	f.e.enqueue(Notification{})
	f.e.enqueue(Notification{})
	if f.e.dropped.Load() != 1 {
		t.Fatal("queue not bounded")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not stop")
	}
}
func TestConcurrentTicksCannotDuplicate(t *testing.T) {
	f := newFixture(t, 0)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.e.Evaluate(context.Background(), f.now) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("duplicate event")
	}
}
func TestLatestReadErrorDoesNotResolve(t *testing.T) {
	f := newFixture(t, 0)
	f.tick(t)
	f.latest.err = errors.New("read failure")
	if f.e.Evaluate(context.Background(), f.now) == nil {
		t.Fatal("lost read error")
	}
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("read error resolved")
	}
}

func TestPendingResetsOnIdentityChange(t *testing.T) {
	f := newFixture(t, 60)
	f.tick(t)
	f.advance(59)
	f.exec(t, "UPDATE nodes SET token_hash=x'02',run_generation='2',report_session_id='new' WHERE id=?", f.node)
	f.value(90)
	f.latest.report.RunGeneration = 2
	f.latest.report.SessionID = "new"
	f.tick(t)
	f.advance(1)
	f.value(90)
	f.latest.report.RunGeneration = 2
	f.latest.report.SessionID = "new"
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("new identity inherited pending duration")
	}
	f.advance(59)
	f.value(90)
	f.latest.report.RunGeneration = 2
	f.latest.report.SessionID = "new"
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("new identity should fire after its own duration")
	}
}

func TestPendingResetsOnCredentialEpochChange(t *testing.T) {
	f := newFixture(t, 60)
	f.tick(t)
	f.advance(59)
	f.exec(t, "UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?", f.node)
	f.value(90)
	f.tick(t)
	f.advance(1)
	f.value(90)
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("same-token replacement inherited prior credential pending duration")
	}
	f.advance(59)
	f.value(90)
	f.tick(t)
	if len(f.events(t, "firing")) != 1 {
		t.Fatal("new credential epoch did not establish its own pending duration")
	}
}
func TestFailedEventWriteDoesNotNotify(t *testing.T) {
	f := newFixture(t, 0)
	f.exec(t, `CREATE TRIGGER reject_alert BEFORE INSERT ON alert_events BEGIN SELECT RAISE(ABORT,'event failure'); END`)
	if err := f.e.Evaluate(context.Background(), f.now); err == nil {
		t.Fatal("lost event write failure")
	}
	if len(f.e.queue) != 0 || len(f.events(t, "all")) != 0 {
		t.Fatal("failed commit notified or persisted")
	}
}

func TestFailedResolutionRetriesBeforeRebreach(t *testing.T) {
	for _, seconds := range []int{0, 60} {
		t.Run((time.Duration(seconds) * time.Second).String(), func(t *testing.T) {
			f := newFixture(t, seconds)
			f.tick(t)
			if seconds > 0 {
				f.advance(seconds)
				f.value(90)
				f.tick(t)
			}
			events := f.events(t, "firing")
			if len(events) != 1 {
				t.Fatalf("expected initial firing episode: %+v", events)
			}
			old := events[0]
			select {
			case <-f.e.queue:
			default:
				t.Fatal("missing initial firing notification")
			}
			f.advance(30)
			recoveredAt := f.now
			f.value(10)
			f.exec(t, `CREATE TRIGGER reject_resolution BEFORE UPDATE ON alert_events BEGIN SELECT RAISE(ABORT,'injected resolution failure'); END`)
			if err := f.e.Evaluate(context.Background(), f.now); err == nil || !strings.Contains(err.Error(), "injected resolution failure") {
				t.Fatalf("expected recovery write failure, got %v", err)
			}
			// The condition breaches again while the old resolution still fails.
			f.advance(30)
			f.value(90)
			if err := f.e.Evaluate(context.Background(), f.now); err == nil || !strings.Contains(err.Error(), "injected resolution failure") {
				t.Fatalf("re-breach did not retry the observed recovery: %v", err)
			}
			if events := f.events(t, "all"); len(events) != 1 || events[0].ID != old.ID || events[0].State != "firing" || len(f.e.queue) != 0 {
				t.Fatalf("failed resolution started or notified another episode: %+v", events)
			}
			if len(f.e.pending) != 0 || len(f.e.resolutions) != 1 {
				t.Fatal("failed resolution did not retain exactly one intent without starting pending")
			}
			f.exec(t, "DROP TRIGGER reject_resolution")
			f.advance(30)
			f.value(90)
			newStart := f.now
			f.tick(t)
			resolved := f.events(t, "resolved")
			if len(resolved) != 1 || resolved[0].ID != old.ID || resolved[0].ResolvedAt == nil || !resolved[0].ResolvedAt.Equal(recoveredAt) || resolved[0].Value == nil || *resolved[0].Value != 10 {
				t.Fatalf("retry lost the original recovery: %+v", resolved)
			}
			if len(f.e.resolutions) != 0 {
				t.Fatal("committed resolution retained its intent")
			}
			select {
			case n := <-f.e.queue:
				if n.EventID != old.ID || n.State != "resolved" || n.ResolvedAt == nil || !n.ResolvedAt.Equal(recoveredAt) || n.Value == nil || *n.Value != 10 {
					t.Fatalf("incorrect recovery notification: %+v", n)
				}
			default:
				t.Fatal("missing recovery notification")
			}
			if seconds > 0 {
				if events := f.events(t, "firing"); len(events) != 0 {
					t.Fatalf("new episode inherited elapsed time: %+v", events)
				}
				f.advance(seconds)
				f.value(90)
				f.tick(t)
			}
			firing := f.events(t, "firing")
			if len(firing) != 1 || firing[0].ID == old.ID || !firing[0].StartedAt.Equal(newStart) {
				t.Fatalf("re-breach merged episodes: %+v", firing)
			}
			f.tick(t)
			if events := f.events(t, "all"); len(events) != 2 {
				t.Fatalf("retry duplicated episodes: %+v", events)
			}
		})
	}
}

func TestResolutionIntentsPrunedAfterDeletion(t *testing.T) {
	for _, removed := range []string{"event", "rule", "node"} {
		t.Run(removed, func(t *testing.T) {
			f := newFixture(t, 0)
			f.tick(t)
			f.advance(1)
			f.value(10)
			f.exec(t, `CREATE TRIGGER reject_resolution BEFORE UPDATE ON alert_events BEGIN SELECT RAISE(ABORT,'injected resolution failure'); END`)
			if err := f.e.Evaluate(context.Background(), f.now); err == nil || len(f.e.resolutions) != 1 {
				t.Fatalf("expected one failed resolution intent, got %v, %+v", err, f.e.resolutions)
			}
			switch removed {
			case "event":
				f.exec(t, "DELETE FROM alert_events")
			case "rule":
				if err := f.s.Delete(context.Background(), f.rule.ID); err != nil {
					t.Fatal(err)
				}
			case "node":
				f.exec(t, "DELETE FROM nodes WHERE id=?", f.node)
			}
			f.advance(1)
			f.value(10)
			err := f.e.Evaluate(context.Background(), f.now)
			if removed == "event" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "injected resolution failure") {
				t.Fatalf("orphaned event was not retried: %v", err)
			}
			if len(f.e.resolutions) != 0 {
				t.Fatalf("removed %s retained resolution intents: %+v", removed, f.e.resolutions)
			}
			f.exec(t, "DROP TRIGGER reject_resolution")
			f.tick(t)
			if len(f.events(t, "firing")) != 0 {
				t.Fatal("orphaned event did not resolve after retry")
			}
		})
	}
}

func TestPendingResetSurvivesOtherEventWriteFailure(t *testing.T) {
	for _, write := range []string{"INSERT", "UPDATE"} {
		for _, reset := range []string{"missing", "recovered", "deleted"} {
			for _, order := range []string{"before", "after"} {
				t.Run(write+"/"+reset+"/"+order, func(t *testing.T) {
					f := newFixture(t, 60)
					start := f.now
					b, err := f.s.Save(context.Background(), "", patch(t, `{"name":"load hot","enabled":true,"scope_kind":"all","metric":"load1","comparator":">","threshold":1,"for_seconds":0,"severity":"warning"}`))
					if err != nil {
						t.Fatal(err)
					}
					// Force both processing orders independently of generated IDs.
					created := start.Add(time.Second)
					if order == "after" {
						created = start.Add(-time.Second)
					}
					f.exec(t, "UPDATE alert_rules SET created_at=? WHERE id=?", timekey.Format(created), b.ID)
					setLoad := func(v float64) {
						f.latest.report.Metrics.Load1 = ptr(v)
						f.latest.report.Metrics.MetricStatus["load1"] = "ok"
						f.latest.report.Metrics.Sources["load"] = v1.SourceMetadata{SampleID: "load", SourceIntervalMS: 10000}
					}
					setLoad(0)
					if write == "UPDATE" {
						setLoad(2)
					}
					f.tick(t)
					f.advance(30)
					f.value(90)
					setLoad(2)
					if write == "UPDATE" {
						setLoad(0)
					}
					switch reset {
					case "missing":
						f.latest.report.Metrics.CPUPercent = nil
					case "recovered":
						f.latest.report.Metrics.CPUPercent = ptr(10.0)
					case "deleted":
						if err := f.s.Delete(context.Background(), f.rule.ID); err != nil {
							t.Fatal(err)
						}
					}
					f.exec(t, `CREATE TRIGGER reject_alert BEFORE `+write+` ON alert_events WHEN NEW.rule_id='`+b.ID+`' BEGIN SELECT RAISE(ABORT,'injected event failure'); END`)
					if err := f.e.Evaluate(context.Background(), f.now); err == nil || !strings.Contains(err.Error(), "injected event failure") {
						t.Fatalf("expected injected write failure, got %v", err)
					}
					if _, exists := f.e.pending[pair{f.rule.ID, f.node}]; exists {
						t.Fatal("unrelated write failure lost pending reset/prune")
					}
					f.exec(t, "DROP TRIGGER reject_alert")
					f.advance(30)
					f.value(90)
					setLoad(2)
					if write == "UPDATE" {
						setLoad(0)
					}
					f.tick(t)
					events := f.events(t, "all")
					if len(events) != 1 || events[0].RuleID != b.ID || (write == "UPDATE" && events[0].State != "resolved") {
						t.Fatalf("failed pair did not retry: %+v", events)
					}
					for _, event := range events {
						if event.RuleID == f.rule.ID {
							t.Fatal("CPU fired using pending time from before the gap")
						}
						if write == "INSERT" && !event.StartedAt.Equal(start.Add(30*time.Second)) {
							t.Fatalf("failed pair lost its pending start: %v", event.StartedAt)
						}
					}
					if reset != "deleted" {
						f.advance(60)
						f.value(90)
						setLoad(0)
						f.tick(t)
						fired := false
						for _, event := range f.events(t, "firing") {
							if event.RuleID == f.rule.ID {
								fired = event.StartedAt.Equal(start.Add(60 * time.Second))
							}
						}
						if !fired {
							t.Fatal("CPU did not fire after a fresh continuous duration")
						}
					}
				})
			}
		}
	}
}
func TestFireRevalidatesIdentityAndDisabledRules(t *testing.T) {
	for _, mode := range []string{"identity", "credential_epoch", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, 0)
			inventory, err := f.e.readNodes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "identity" {
				f.exec(t, "UPDATE nodes SET token_hash=x'02' WHERE id=?", f.node)
			} else if mode == "credential_epoch" {
				f.exec(t, "UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?", f.node)
			} else {
				f.exec(t, "UPDATE alert_rules SET enabled=0 WHERE id=?", f.rule.ID)
			}
			if err = f.e.fire(context.Background(), f.rule, inventory[0], ptr(90.0), f.now); err != nil {
				t.Fatal(err)
			}
			if len(f.events(t, "all")) != 0 || len(f.e.queue) != 0 {
				t.Fatal("stale evaluation committed after identity/disable")
			}
		})
	}
}
func TestRealIngestIdentityFence(t *testing.T) {
	f := newFixture(t, 0)
	real := ingest.NewService(f.db, ingest.Options{Now: func() time.Time { return f.now }})
	f.e = NewEvaluator(f.s, real, EvaluatorOptions{})
	f.exec(t, "UPDATE nodes SET run_generation='1',report_session_id='one' WHERE id=?", f.node)
	var report v1.Report
	raw := `{"protocol_version":1,"run_generation":1,"session_id":"one","seq":1,"sampled_at":"2026-10-27T12:00:00Z","runtime":{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[]},"metrics":{"cpu_percent":90,"load1":0,"load5":0,"load15":0,"memory_total_bytes":100,"memory_available_bytes":50,"swap_total_bytes":0,"swap_used_bytes":0,"uptime_seconds":10,"disks":[],"networks":[],"metric_status":{"cpu_percent":"ok","load1":"ok","load5":"ok","load15":"ok","memory_total_bytes":"ok","memory_available_bytes":"ok","swap_total_bytes":"ok","swap_used_bytes":"ok","uptime_seconds":"ok"},"sources":{"cpu":{"sample_id":"cpu-1","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":10000},"load":{"sample_id":"load-1","age_ms":0,"source_interval_ms":10000},"memory":{"sample_id":"memory-1","age_ms":0,"source_interval_ms":10000},"swap":{"sample_id":"swap-1","age_ms":0,"source_interval_ms":10000},"uptime":{"sample_id":"uptime-1","age_ms":0,"source_interval_ms":10000}}},"traffic":[]}`
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("A", 43)
	credential := sha256.Sum256([]byte(token))
	f.exec(t, "UPDATE nodes SET token_hash=? WHERE id=?", credential[:], f.node)
	admission, err := real.AdmitToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := real.AcceptAdmitted(context.Background(), admission, report); err != nil {
		t.Fatal(err)
	}
	// Rotating the credential fences the old in-memory metric before evaluation.
	f.exec(t, "UPDATE nodes SET token_hash=x'02' WHERE id=?", f.node)
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("identity-fenced old metric fired")
	}
}

type metadataEditReader struct {
	latest *latestStub
	edit   func()
}

func (s *metadataEditReader) Latest(ctx context.Context, id string) (ingest.LatestReport, error) {
	if s.edit != nil {
		edit := s.edit
		s.edit = nil
		edit()
	}
	return s.latest.Latest(ctx, id)
}
func TestConcurrentMetadataEditPreservesPendingDuration(t *testing.T) {
	f := newFixture(t, 60)
	reader := &metadataEditReader{latest: f.latest}
	f.e = NewEvaluator(f.s, reader, EvaluatorOptions{})
	f.tick(t)
	f.advance(60)
	f.value(90)
	reader.edit = func() {
		if _, err := f.s.Save(context.Background(), f.rule.ID, patch(t, `{"name":"new name","severity":"critical"}`)); err != nil {
			t.Fatal(err)
		}
	}
	f.tick(t)
	if len(f.events(t, "all")) != 0 {
		t.Fatal("stale rule version should skip insertion")
	}
	f.advance(1)
	f.value(90)
	f.tick(t)
	events := f.events(t, "firing")
	if len(events) != 1 || events[0].RuleName != "new name" || !events[0].StartedAt.Equal(f.now.Add(-61*time.Second)) {
		t.Fatalf("metadata edit lost pending duration: %+v", events)
	}
}
