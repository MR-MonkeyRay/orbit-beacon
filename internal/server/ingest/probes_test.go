package ingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func newProbeFixture(t *testing.T) *coalesceFixture {
	return newCoalesceFixture(t, 30*time.Second, "https://example.test/health", "tcp://example.test:443", "tcp://old.example.test:443", "tcp://new.example.test:443")
}

func probeResult(target string, checkedAt time.Time, ok bool) v1.ProbeResult {
	result := v1.ProbeResult{Target: target, OK: ok, CheckedAt: checkedAt}
	if ok {
		latency := 12.5
		result.LatencyMS = &latency
	} else {
		result.Error = "timeout"
	}
	return result
}

func probeAccept(t *testing.T, f *coalesceFixture) (v1.ReportResponse, error) {
	t.Helper()
	return f.service.accept(context.Background(), f.id, nil, 0, f.report)
}

func probeTotals(t *testing.T, f *coalesceFixture, target string) (checks, failures int, latest string) {
	t.Helper()
	if err := f.db.QueryRow(`SELECT checks,failures FROM probe_hourly WHERE node_id=? AND target=?`, f.id, target).Scan(&checks, &failures); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT checked_at FROM probe_latest WHERE node_id=? AND target=?`, f.id, target).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	return checks, failures, latest
}

func TestProbeCheckedAtDedupeCadenceAndRestart(t *testing.T) {
	f := newProbeFixture(t)
	target := "https://example.test/health"
	first := f.now.Add(time.Second)
	f.now = first
	f.report.Seq++
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, first, false)}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.db.QueryRow(`SELECT count(*) FROM probe_latest`).Scan(&rows); err != nil || rows != 0 || f.writes(t) != 1 {
		t.Fatalf("fast report persisted probes: rows=%d writes=%d err=%v", rows, f.writes(t), err)
	}

	// Repeats and out-of-order results do not add checks; a newer check does.
	for _, checkedAt := range []time.Time{first, first.Add(2 * time.Second), first.Add(time.Second)} {
		f.now = f.now.Add(time.Second)
		f.report.Seq++
		f.report.ProbeResults = []v1.ProbeResult{probeResult(target, checkedAt, checkedAt.Equal(first.Add(2*time.Second)))}
		if _, err := probeAccept(t, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM probe_latest`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("probe write before cadence: rows=%d err=%v", rows, err)
	}

	f.now = first.Add(30 * time.Second)
	f.report.Seq++
	f.report.ProbeResults = nil
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	checks, failures, latest := probeTotals(t, f, target)
	if checks != 2 || failures != 1 || latest != timekey.Format(first.Add(2*time.Second)) {
		t.Fatalf("checks=%d failures=%d latest=%s", checks, failures, latest)
	}

	// A fresh service seeds the durable watermark and does not count the repeat.
	f.service = NewService(f.db, Options{Now: func() time.Time { return f.now }, PersistInterval: 30 * time.Second})
	f.report.Seq++
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, first.Add(2*time.Second), true)}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	checks, failures, _ = probeTotals(t, f, target)
	if checks != 2 || failures != 1 {
		t.Fatalf("restart recounted result: checks=%d failures=%d", checks, failures)
	}
}

func TestProbeDrainPersistsPendingAndOwnsCallerPointers(t *testing.T) {
	f := newProbeFixture(t)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	latency, status := 7.25, 204
	target := "https://example.test/health"
	f.report.ProbeResults = []v1.ProbeResult{{Target: target, OK: true, LatencyMS: &latency, StatusCode: &status, CheckedAt: f.now}}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	latency, status = 999, 599
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var gotLatency float64
	var gotStatus int
	if err := f.db.QueryRow(`SELECT latency_ms,status_code FROM probe_latest WHERE node_id=? AND target=?`, f.id, target).Scan(&gotLatency, &gotStatus); err != nil || gotLatency != 7.25 || gotStatus != 204 {
		t.Fatalf("latency=%v status=%d err=%v", gotLatency, gotStatus, err)
	}
}

func TestProbePersistFailureRetainsAcceptedPending(t *testing.T) {
	f := newProbeFixture(t)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	target := "tcp://example.test:443"
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, f.now, false)}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_probe BEFORE INSERT ON probe_hourly BEGIN SELECT RAISE(ABORT,'probe write failed'); END`); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(30 * time.Second)
	f.report.Seq++
	f.report.ProbeResults = nil
	if _, err := probeAccept(t, f); err == nil {
		t.Fatal("probe persistence failure was accepted")
	}
	state := f.service.acquireExistingState(f.id)
	defer f.service.releaseState(state)
	state.mu.Lock()
	pending := len(state.probes.targets[target].pending)
	state.mu.Unlock()
	if pending != 1 || f.writes(t) != 1 {
		t.Fatalf("pending=%d writes=%d", pending, f.writes(t))
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_probe`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks, failures, _ := probeTotals(t, f, target)
	if checks != 1 || failures != 1 {
		t.Fatalf("retry checks=%d failures=%d", checks, failures)
	}
}

func TestProbeShutdownIdentityFences(t *testing.T) {
	for _, mutation := range []string{"none", "delete", "revoke", "disable", "credential_epoch", "generation", "session"} {
		t.Run(mutation, func(t *testing.T) {
			f := newProbeFixture(t)
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			f.report.ProbeResults = []v1.ProbeResult{probeResult("tcp://example.test:443", f.now, false)}
			if _, err := probeAccept(t, f); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mutation {
			case "delete":
				err = f.nodes.Delete(context.Background(), f.id, true)
			case "revoke":
				err = f.nodes.RevokeCredential(context.Background(), f.id)
			case "disable":
				err = f.nodes.SetDisabled(context.Background(), f.id, true)
			case "credential_epoch":
				_, err = f.db.Exec(`UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?`, f.id)
			case "generation":
				_, err = f.db.Exec(`UPDATE nodes SET run_generation='2',report_session_id='new' WHERE id=?`, f.id)
			case "session":
				_, err = f.db.Exec(`UPDATE nodes SET report_session_id='replacement' WHERE id=?`, f.id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM probe_hourly`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mutation == "none" {
				want = 1
			}
			if count != want {
				t.Fatalf("fenced %s: count=%d want=%d", mutation, count, want)
			}
		})
	}
}

func TestProbeBufferTargetAndPendingBounds(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	credential := sha256.Sum256([]byte("credential"))
	b := probeBuffer{credential: credential, generation: 1, session: "session", targets: make(map[string]probeTargetBuffer)}
	for n := 0; n < probeTargetLimit; n++ {
		target := fmt.Sprintf("tcp://host-%d.test:443", n)
		b.targets[target] = probeTargetBuffer{counted: now, pending: []v1.ProbeResult{probeResult(target, now, true)}}
	}
	if err := b.buffer(context.Background(), nil, "node", []v1.ProbeResult{probeResult("tcp://overflow.test:443", now.Add(time.Second), true)}, []string{"tcp://overflow.test:443"}); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("target overflow error=%v", err)
	}
	if len(b.targets) != probeTargetLimit {
		t.Fatalf("targets=%d", len(b.targets))
	}

	target := "tcp://full.test:443"
	pending := make([]v1.ProbeResult, probePendingLimit)
	for n := range pending {
		pending[n] = probeResult(target, now.Add(time.Duration(n+1)*time.Second), true)
	}
	b = probeBuffer{credential: credential, generation: 1, session: "session", targets: map[string]probeTargetBuffer{
		target: {counted: pending[len(pending)-1].CheckedAt, pending: pending},
	}}
	if err := b.buffer(context.Background(), nil, "node", []v1.ProbeResult{probeResult(target, now.Add(time.Hour), true)}, []string{target}); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("pending overflow error=%v", err)
	}
	if len(b.targets[target].pending) != probePendingLimit {
		t.Fatalf("pending=%d", len(b.targets[target].pending))
	}
}

func TestProbeFastPathAuthenticationFences(t *testing.T) {
	for _, mutation := range []string{"delete", "revoke", "disable"} {
		t.Run(mutation, func(t *testing.T) {
			f := newProbeFixture(t)
			var err error
			switch mutation {
			case "delete":
				err = f.nodes.Delete(context.Background(), f.id, true)
			case "revoke":
				err = f.nodes.RevokeCredential(context.Background(), f.id)
			case "disable":
				err = f.nodes.SetDisabled(context.Background(), f.id, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			f.report.ProbeResults = []v1.ProbeResult{probeResult("tcp://example.test:443", f.now, false)}
			_, err = probeAccept(t, f)
			want := nodes.ErrUnauthenticated
			if mutation == "disable" {
				want = nodes.ErrDisabled
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want %v", err, want)
			}
		})
	}
}

func TestProbeOverlappingWritersRebasePartialQueue(t *testing.T) {
	f := newProbeFixture(t)
	target := "tcp://example.test:443"
	t1, t2 := f.now.Add(time.Second), f.now.Add(2*time.Second)
	f.now = t1
	f.report.Seq++
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, t1, false)}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	f.now = t2
	f.report.Seq++
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, t2, true)}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}

	// A second service shares the same durable process identity and commits t1.
	other := NewService(f.db, Options{Now: func() time.Time { return f.now }, PersistInterval: 30 * time.Second})
	report := f.report
	report.Seq++
	report.ProbeResults = []v1.ProbeResult{probeResult(target, t1, false)}
	if _, err := other.accept(context.Background(), f.id, nil, 0, report); err != nil {
		t.Fatal(err)
	}
	if err := other.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks, failures, latest := probeTotals(t, f, target)
	if checks != 2 || failures != 1 || latest != timekey.Format(t2) {
		t.Fatalf("checks=%d failures=%d latest=%s", checks, failures, latest)
	}
}

func TestProbeProcessAdvanceRetriesPriorAndIncomingAtomically(t *testing.T) {
	for _, failure := range []string{"probe", "node_update", "commit"} {
		t.Run(failure, func(t *testing.T) {
			f := newProbeFixture(t)
			oldTarget, newTarget := "tcp://old.example.test:443", "tcp://new.example.test:443"
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			f.report.ProbeResults = []v1.ProbeResult{probeResult(oldTarget, f.now, false)}
			if _, err := probeAccept(t, f); err != nil {
				t.Fatal(err)
			}
			f.report.RunGeneration = 2
			f.report.SessionID = "restarted"
			f.report.Seq = 1
			f.report.ProbeResults = []v1.ProbeResult{probeResult(newTarget, f.now.Add(time.Second), true)}
			var trigger string
			switch failure {
			case "probe":
				trigger = `CREATE TRIGGER fail_probe_restart BEFORE INSERT ON probe_hourly WHEN NEW.target='tcp://new.example.test:443' BEGIN SELECT RAISE(ABORT,'new probe failed'); END`
			case "node_update":
				trigger = `CREATE TRIGGER fail_probe_restart BEFORE UPDATE OF run_generation ON nodes WHEN NEW.run_generation='2' BEGIN SELECT RAISE(ABORT,'node update failed'); END`
			case "commit":
				trigger = `CREATE TABLE probe_restart_guard(node_id TEXT REFERENCES nodes(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_probe_restart AFTER INSERT ON probe_hourly WHEN NEW.target='tcp://new.example.test:443' BEGIN INSERT INTO probe_restart_guard VALUES('missing-node'); END`
			}
			if _, err := f.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := probeAccept(t, f); err == nil {
				t.Fatal("failed process advance committed")
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM probe_hourly`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial probe transaction: count=%d err=%v", count, err)
			}
			state := f.service.acquireExistingState(f.id)
			state.mu.Lock()
			oldPending := len(state.probes.targets[oldTarget].pending)
			state.mu.Unlock()
			f.service.releaseState(state)
			if oldPending != 1 {
				t.Fatalf("prior accepted probe pending=%d", oldPending)
			}
			if _, err := f.db.Exec(`DROP TRIGGER fail_probe_restart`); err != nil {
				t.Fatal(err)
			}
			if _, err := probeAccept(t, f); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`SELECT count(*) FROM probe_hourly`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("retry count=%d err=%v", count, err)
			}
			var generation, session string
			if err := f.db.QueryRow(`SELECT run_generation,report_session_id FROM nodes WHERE id=?`, f.id).Scan(&generation, &session); err != nil || generation != "2" || session != "restarted" {
				t.Fatalf("identity=%s/%s err=%v", generation, session, err)
			}
		})
	}
}

func TestProbeCredentialRotationAfterInitialReadDropsOldPending(t *testing.T) {
	f := newProbeFixture(t)
	oldTarget, newTarget := "tcp://old.example.test:443", "tcp://new.example.test:443"
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.ProbeResults = []v1.ProbeResult{probeResult(oldTarget, f.now, false)}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	f.report.RunGeneration = 2
	f.report.SessionID = "new-credential"
	f.report.Seq = 1
	f.report.ProbeResults = []v1.ProbeResult{probeResult(newTarget, f.now.Add(time.Second), true)}
	rotated := sha256.Sum256([]byte("rotated-during-probe-read"))
	var once bool
	f.service.now = func() time.Time {
		if !once {
			once = true
			if _, err := f.db.Exec(`UPDATE nodes SET token_hash=? WHERE id=?`, rotated[:], f.id); err != nil {
				t.Fatal(err)
			}
		}
		return f.now
	}
	if _, err := probeAccept(t, f); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Fatalf("queued old-credential report error=%v", err)
	}
	var oldCount, newCount int
	if err := f.db.QueryRow(`SELECT count(*) FROM probe_hourly WHERE target=?`, oldTarget).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM probe_hourly WHERE target=?`, newTarget).Scan(&newCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || newCount != 0 {
		t.Fatalf("old credential carried probes: old=%d new=%d", oldCount, newCount)
	}
}

func TestRuntimeTargetRemovalPurgesPendingAtomicallyAndDuplicatesCannotRestore(t *testing.T) {
	target := "tcp://removed.example:443"
	f := newCoalesceFixture(t, 30*time.Second, target)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, f.now, true)}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	old := f.report
	f.report.Seq++
	f.report.Runtime.ProbeTargets = []string{}
	f.report.ProbeResults = nil
	if _, err := f.db.Exec(`CREATE TRIGGER reject_runtime_removal BEFORE UPDATE OF agent_runtime ON nodes BEGIN SELECT RAISE(ABORT,'runtime rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := probeAccept(t, f); err == nil {
		t.Fatal("failed runtime transaction accepted")
	}
	runtime, err := f.nodes.GetRuntime(context.Background(), f.id)
	if err != nil || runtime == nil || len(runtime.ProbeTargets) != 1 || runtime.ProbeTargets[0] != target {
		t.Fatalf("rollback runtime=%+v err=%v", runtime, err)
	}
	state := f.service.acquireExistingState(f.id)
	state.mu.Lock()
	pending := len(state.probes.targets[target].pending)
	state.mu.Unlock()
	f.service.releaseState(state)
	if pending != 1 {
		t.Fatalf("rollback discarded accepted pending probes=%d", pending)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_runtime_removal`); err != nil {
		t.Fatal(err)
	}
	if _, err := probeAccept(t, f); err != nil {
		t.Fatal(err)
	}
	f.report = old
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, f.now.Add(time.Second), true)}
	response, err := probeAccept(t, f)
	if err != nil || response.AcceptedSeq != 3 {
		t.Fatalf("duplicate response=%+v err=%v", response, err)
	}
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime, err = f.nodes.GetRuntime(context.Background(), f.id)
	if err != nil || runtime == nil || len(runtime.ProbeTargets) != 0 {
		t.Fatalf("duplicate restored runtime=%+v err=%v", runtime, err)
	}
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		var count int
		if err := f.db.QueryRow("SELECT count(*) FROM "+table+" WHERE node_id=?", f.id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s resurrected target: count=%d err=%v", table, count, err)
		}
	}
}
