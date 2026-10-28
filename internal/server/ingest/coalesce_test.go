package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

const coalesceToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type coalesceFixture struct {
	db      *sql.DB
	nodes   *nodes.Service
	service *Service
	id      string
	now     time.Time
	report  v1.Report
}

func newCoalesceFixture(t *testing.T, interval time.Duration, targets ...string) *coalesceFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &coalesceFixture{db: db, now: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	f.nodes = nodes.NewService(db, nodes.Options{Now: clock})
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "coalesce"})
	if err != nil {
		t.Fatal(err)
	}
	f.id = node.ID
	credential := sha256.Sum256([]byte("coalesce-test-credential"))
	if _, err := db.Exec(`UPDATE nodes SET token_hash=?,status='unknown',first_enrolled_at=?,public_settings='{"enabled":true,"name":"Coalesce"}' WHERE id=?`, credential[:], timekey.Format(f.now), f.id); err != nil {
		t.Fatal(err)
	}
	// Count row persistence, including identical-value UPDATEs.
	if _, err := db.Exec(`CREATE TABLE report_writes (n INTEGER NOT NULL); INSERT INTO report_writes VALUES(0); CREATE TRIGGER count_report_writes AFTER UPDATE OF last_seen ON nodes BEGIN UPDATE report_writes SET n=n+1; END`); err != nil {
		t.Fatal(err)
	}
	f.service = NewService(db, Options{Now: clock, PersistInterval: interval})
	cpu, load, uptime := 10.0, 0.5, 100.0
	total, available, zero := uint64(8192), uint64(4096), uint64(0)
	window := int64(10000)
	m := v1.Metrics{CPUPercent: &cpu, Load1: &load, Load5: &load, Load15: &load, UptimeSeconds: &uptime, MemoryTotalBytes: &total, MemoryAvailableBytes: &available, SwapTotalBytes: &zero, SwapUsedBytes: &zero, Disks: []v1.DiskMetrics{}, Networks: []v1.NetworkMetrics{}, MetricStatus: map[string]string{}, Sources: map[string]v1.SourceMetadata{}}
	for _, name := range []string{"cpu_percent", "load1", "load5", "load15", "uptime_seconds", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes"} {
		m.MetricStatus[name] = "ok"
	}
	for _, name := range []string{"cpu", "load", "memory", "swap", "uptime"} {
		m.Sources[name] = v1.SourceMetadata{SampleID: name, SourceIntervalMS: 10000}
	}
	source := m.Sources["cpu"]
	source.MeasurementWindowMS = &window
	m.Sources["cpu"] = source
	f.report = v1.Report{ProtocolVersion: 1, RunGeneration: 1, SessionID: "session", Seq: 1, SampledAt: f.now, Runtime: v1.DefaultRuntimeSettings(), System: &v1.System{Hostname: "host", OS: "linux", Arch: "amd64"}, Metrics: m, Traffic: []v1.Traffic{}}
	if len(targets) != 0 {
		f.report.Runtime.ProbeTargets = targets
	}
	if _, err := f.service.Accept(context.Background(), f.id, f.report); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *coalesceFixture) writes(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow("SELECT n FROM report_writes").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCoalesceDurabilityConditions(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *coalesceFixture)
		write  bool
	}{
		{"equal system hash", func(t *testing.T, f *coalesceFixture) {}, false},
		{"nil system unchanged", func(t *testing.T, f *coalesceFixture) { f.report.System = nil }, false},
		{"system changed", func(t *testing.T, f *coalesceFixture) { f.report.System.Hostname = "changed" }, true},
		{"new run", func(t *testing.T, f *coalesceFixture) { f.report.RunGeneration++; f.report.SessionID = "next" }, true},
		{"restart", func(t *testing.T, f *coalesceFixture) {
			f.service = NewService(f.db, Options{Now: func() time.Time { return f.now }})
		}, true},
		{"runtime changed", func(t *testing.T, f *coalesceFixture) { f.report.Runtime.ReportIntervalSeconds = 5 }, true},
		{"interval boundary", func(t *testing.T, f *coalesceFixture) { f.now = f.now.Add(time.Second) }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCoalesceFixture(t, 2*time.Second)
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			test.mutate(t, f)
			_, err := f.service.Accept(context.Background(), f.id, f.report)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if test.write {
				want++
			}
			if got := f.writes(t); got != want {
				t.Fatalf("writes=%d want %d", got, want)
			}
			latest, err := f.service.Latest(context.Background(), f.id)
			if err != nil || latest.Seq != 2 || !latest.ReceivedAt.Equal(f.now) {
				t.Fatalf("latest=%+v err=%v", latest, err)
			}
		})
	}
}

func TestCoalesceRuntimeChangeAndDuplicate(t *testing.T) {
	f := newCoalesceFixture(t, 0)
	ctx := context.Background()
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Runtime.ReportIntervalSeconds = 5
	if _, err := f.service.Accept(ctx, f.id, f.report); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Runtime.ReportIntervalSeconds = 10
	if _, err := f.service.Accept(ctx, f.id, f.report); err != nil {
		t.Fatal(err)
	}
	if f.writes(t) != 3 {
		t.Fatal("runtime changes did not persist")
	}
	latest, err := f.service.Latest(ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq--
	response, err := f.service.Accept(ctx, f.id, f.report)
	if err != nil || response.AcceptedSeq != latest.Seq {
		t.Fatalf("duplicate response=%+v err=%v", response, err)
	}
	after, err := f.service.Latest(ctx, f.id)
	if err != nil || after.Seq != latest.Seq || !after.ReceivedAt.Equal(latest.ReceivedAt) || f.writes(t) != 3 {
		t.Fatalf("duplicate changed snapshot: %+v err=%v", after, err)
	}
	if err := f.service.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	f.report.Seq = 4
	if _, err := f.service.Accept(ctx, f.id, f.report); err != ErrServiceClosed {
		t.Fatalf("closed fast path returned %v", err)
	}
}

func TestCoalesceConcurrentReportsReadersAndPruning(t *testing.T) {
	f := newCoalesceFixture(t, 0)
	f.now = f.now.Add(2 * time.Second)
	ctx := context.Background()
	start := make(chan struct{})
	errors := make(chan error, 16)
	var wait sync.WaitGroup
	for seq := uint64(2); seq <= 4; seq++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			report := f.report
			report.Seq = seq
			if _, err := f.service.Accept(ctx, f.id, report); err != nil {
				errors <- err
			}
		}()
	}
	for range 6 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for range 10 {
				latest, err := f.service.Latest(ctx, f.id)
				if err != nil {
					errors <- err
					return
				}
				if latest.Seq < 1 || latest.Seq > 4 {
					errors <- fmt.Errorf("invalid snapshot seq %d", latest.Seq)
					return
				}
				// Caller mutations must never alter the owned snapshot.
				latest.Metrics.MetricStatus["cpu_percent"] = "error"
				if _, err := json.Marshal(latest); err != nil {
					errors <- err
					return
				}
				out, err := f.service.PublicNode(ctx, f.id, f.now)
				if err != nil || out.Status != "online" {
					errors <- fmt.Errorf("projection=%+v err=%v", out, err)
					return
				}
				if err := f.service.prune(ctx, f.db); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	latest, err := f.service.Latest(ctx, f.id)
	if err != nil || latest.Seq != 4 || !latest.ReceivedAt.Equal(f.now) || latest.Metrics.MetricStatus["cpu_percent"] != "ok" {
		t.Fatalf("final snapshot=%+v err=%v", latest, err)
	}
	if f.writes(t) != 1 {
		t.Fatal("concurrent fast reports persisted a row")
	}
}

func TestCoalesceConcurrentMutationBeforeAuthentication(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(context.Context, *coalesceFixture) error
		want   error
	}{
		{"revoke", func(ctx context.Context, f *coalesceFixture) error { return f.nodes.RevokeCredential(ctx, f.id) }, nodes.ErrUnauthenticated},
		{"disable", func(ctx context.Context, f *coalesceFixture) error { return f.nodes.SetDisabled(ctx, f.id, true) }, nodes.ErrDisabled},
		{"delete", func(ctx context.Context, f *coalesceFixture) error { return f.nodes.Delete(ctx, f.id, true) }, nodes.ErrUnauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCoalesceFixture(t, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			state := f.service.acquireExistingState(f.id)
			defer f.service.releaseState(state)
			state.acceptGate <- struct{}{}
			locked := true
			defer func() {
				if locked {
					<-state.acceptGate
				}
			}()
			result := make(chan error, 1)
			credential := sha256.Sum256([]byte("coalesce-test-credential"))
			go func() {
				_, err := f.service.accept(ctx, f.id, credential[:], 0, f.report, false)
				result <- err
			}()
			// Hold the report before its current-state SELECT, then commit the
			// mutation. Cached credentials or a pre-lock SELECT would accept it.
			for {
				f.service.mu.Lock()
				refs := state.refs
				f.service.mu.Unlock()
				if refs == 2 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("report did not reach the state barrier")
				case <-time.After(time.Millisecond):
				}
			}
			if err := test.mutate(ctx, f); err != nil {
				t.Fatal(err)
			}
			<-state.acceptGate
			locked = false
			select {
			case err := <-result:
				if err != test.want {
					t.Fatalf("report error=%v want %v", err, test.want)
				}
			case <-ctx.Done():
				t.Fatal("report did not finish after releasing state")
			}
			latest, err := f.service.Latest(ctx, f.id)
			if !errors.Is(err, nodes.ErrNotFound) {
				t.Fatalf("Latest exposed invalid identity: snapshot=%+v err=%v", latest, err)
			}
			state.mu.Lock()
			seq, receivedAt := state.seq, state.receivedAt
			state.mu.Unlock()
			if seq != 1 || receivedAt.Equal(f.now) {
				t.Fatal("rejected report advanced the cached snapshot")
			}
		})
	}
}

func TestCoalesceLatestFencesCommittedMutations(t *testing.T) {
	for _, mutation := range []string{"revoke", "disable", "delete", "rotate", "credential_epoch", "generation", "session"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixture(t, 0)
			ctx := context.Background()
			var err error
			switch mutation {
			case "revoke":
				err = f.nodes.RevokeCredential(ctx, f.id)
			case "disable":
				err = f.nodes.SetDisabled(ctx, f.id, true)
			case "delete":
				err = f.nodes.Delete(ctx, f.id, true)
			case "rotate":
				credential := sha256.Sum256([]byte("rotated-credential"))
				_, err = f.db.Exec("UPDATE nodes SET token_hash=? WHERE id=?", credential[:], f.id)
			case "credential_epoch":
				_, err = f.db.Exec("UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?", f.id)
			case "generation":
				_, err = f.db.Exec("UPDATE nodes SET run_generation='2' WHERE id=?", f.id)
			case "session":
				_, err = f.db.Exec("UPDATE nodes SET report_session_id='other' WHERE id=?", f.id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if latest, err := f.service.Latest(ctx, f.id); !errors.Is(err, nodes.ErrNotFound) {
				t.Fatalf("committed %s exposed snapshot=%+v err=%v", mutation, latest, err)
			}
		})
	}
}

func TestCoalesceLatestFencesMutationAfterFastPathRead(t *testing.T) {
	for _, mutation := range []string{"revoke", "disable", "delete", "rotate"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixture(t, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			read := make(chan struct{})
			resume := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(resume) })
			// The acceptance clock is sampled after the plain SELECT. Stop the
			// report there so the mutation commits before its memory update.
			f.service.now = func() time.Time {
				close(read)
				select {
				case <-resume:
				case <-ctx.Done():
				}
				return f.now
			}
			result := make(chan error, 1)
			credential := sha256.Sum256([]byte("coalesce-test-credential"))
			go func() {
				_, err := f.service.accept(ctx, f.id, credential[:], 0, f.report, false)
				result <- err
			}()
			select {
			case <-read:
			case <-ctx.Done():
				t.Fatal("report did not finish the fast-path SELECT")
			}
			var err error
			switch mutation {
			case "revoke":
				err = f.nodes.RevokeCredential(ctx, f.id)
			case "disable":
				err = f.nodes.SetDisabled(ctx, f.id, true)
			case "delete":
				err = f.nodes.Delete(ctx, f.id, true)
			case "rotate":
				rotated := sha256.Sum256([]byte("rotated-credential"))
				_, err = f.db.ExecContext(ctx, "UPDATE nodes SET token_hash=? WHERE id=?", rotated[:], f.id)
			}
			if err != nil {
				t.Fatal(err)
			}
			release.Do(func() { close(resume) })
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("fast report did not finish")
			}
			state := f.service.acquireExistingState(f.id)
			state.mu.Lock()
			seq := state.seq
			state.mu.Unlock()
			f.service.releaseState(state)
			if seq != 2 {
				t.Fatalf("linearized report did not advance sequence: seq=%d", seq)
			}
			if latest, err := f.service.Latest(ctx, f.id); !errors.Is(err, nodes.ErrNotFound) {
				t.Fatalf("post-SELECT %s exposed snapshot=%+v err=%v", mutation, latest, err)
			}
		})
	}
}

func TestCoalesceCredentialABAReclaimsBeforeAndAfterRead(t *testing.T) {
	for _, resetAfterRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset_after_read=%v", resetAfterRead), func(t *testing.T) {
			f := newCoalesceFixture(t, 0)
			ctx := context.Background()
			credential := sha256.Sum256([]byte(coalesceToken))
			if _, err := f.db.Exec("UPDATE nodes SET token_hash=? WHERE id=?", credential[:], f.id); err != nil {
				t.Fatal(err)
			}
			f.report.Seq = 100
			if _, err := f.service.accept(ctx, f.id, nil, 0, f.report, false); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(time.Second)
			reenroll := func() {
				if err := f.nodes.RevokeCredential(ctx, f.id); err != nil {
					t.Fatal(err)
				}
				enrollments := enrollment.NewService(f.db, enrollment.Options{Now: func() time.Time { return f.now }})
				ticket, err := enrollments.CreateSingle(ctx, f.id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := enrollments.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "aba-installation", NodeToken: coalesceToken, System: *f.report.System}); err != nil {
					t.Fatal(err)
				}
				if latest, err := f.service.Latest(ctx, f.id); !errors.Is(err, nodes.ErrNotFound) {
					t.Fatalf("ABA reset exposed old snapshot: latest=%+v err=%v", latest, err)
				}
			}
			if resetAfterRead {
				// Make persistence due, then reuse A after its first SELECT.
				f.now = f.now.Add(30 * time.Second)
				var once sync.Once
				f.service.now = func() time.Time { once.Do(reenroll); return f.now }
			} else {
				reenroll()
			}
			f.report.Seq = 2
			response, err := f.service.accept(ctx, f.id, nil, 0, f.report, false)
			if resetAfterRead {
				if !errors.Is(err, ErrIdentityConflict) {
					t.Fatalf("queued pre-reset report error=%v", err)
				}
				response, err = f.service.accept(ctx, f.id, nil, 0, f.report, false)
			}
			if err != nil || response.AcceptedSeq != 2 {
				t.Fatalf("ABA report deduplicated: response=%+v err=%v", response, err)
			}
			var generation, session string
			if err := f.db.QueryRow("SELECT run_generation, report_session_id FROM nodes WHERE id=?", f.id).Scan(&generation, &session); err != nil {
				t.Fatal(err)
			}
			if generation != "1" || session != f.report.SessionID {
				t.Fatalf("identity not reclaimed: generation=%s session=%q", generation, session)
			}
			f.report.Seq++
			if response, err := f.service.accept(ctx, f.id, nil, 0, f.report, false); err != nil || response.AcceptedSeq != 3 {
				t.Fatalf("sequence history survived ABA: response=%+v err=%v", response, err)
			}
			if latest, err := f.service.Latest(ctx, f.id); err != nil || latest.Seq != 3 {
				t.Fatalf("new sequence dropped after ABA: latest=%+v err=%v", latest, err)
			}
		})
	}
}

func TestFreshCredentialEpochRejectsQueuedExactABA(t *testing.T) {
	f := newCoalesceFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	enrollments := enrollment.NewService(f.db, enrollment.Options{Now: func() time.Time { return f.now }})
	rebind := func() {
		t.Helper()
		if err := f.nodes.RevokeCredential(ctx, f.id); err != nil {
			t.Fatal(err)
		}
		ticket, err := enrollments.CreateSingle(ctx, f.id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enrollments.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "exact-aba", NodeToken: coalesceToken, System: v1.System{Hostname: "enrolled", OS: "linux", Arch: "amd64"}}); err != nil {
			t.Fatal(err)
		}
	}
	rebind()
	// No report has been accepted under this identity: A/0/'' is the exact
	// durable identity both before and after the revoke/rebind below.
	f.service = NewService(f.db, Options{Now: func() time.Time { return f.now }})
	target := "tcp://queued-identity.test:443"
	f.report.Runtime.ProbeTargets = []string{target}
	f.report.ProbeResults = []v1.ProbeResult{probeResult(target, f.now, true)}
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now, "100", "200")}
	f.report.System.Hostname = "queued-old-host"
	admission, err := f.service.AdmitToken(ctx, coalesceToken)
	if err != nil {
		t.Fatal(err)
	}
	releaseWrite, err := f.service.beginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	defer release.Do(releaseWrite)
	result := make(chan error, 1)
	go func() {
		_, err := f.service.AcceptAdmitted(ctx, admission, f.report)
		result <- err
	}()
	for len(f.service.writerQueue) != 2 {
		select {
		case <-ctx.Done():
			t.Fatal("first report did not queue before its transaction")
		case <-time.After(time.Millisecond):
		}
	}
	rebind()
	release.Do(releaseWrite)
	select {
	case err := <-result:
		if !errors.Is(err, ErrIdentityConflict) && !errors.Is(err, nodes.ErrUnauthenticated) {
			t.Fatalf("queued exact-ABA report error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("queued report did not finish")
	}
	var runtime, system, generation, session string
	var seen sql.NullString
	if err := f.db.QueryRow(`SELECT agent_runtime,system,last_seen,run_generation,report_session_id FROM nodes WHERE id=?`, f.id).Scan(&runtime, &system, &seen, &generation, &session); err != nil {
		t.Fatal(err)
	}
	if runtime != "" || seen.Valid || generation != "0" || session != "" || system != `{"hostname":"enrolled","os":"linux","arch":"amd64"}` {
		t.Fatalf("queued report resurrected identity: runtime=%q system=%q seen=%v run=%s/%q", runtime, system, seen, generation, session)
	}
	for _, table := range []string{"probe_latest", "probe_hourly", "traffic_hourly", "traffic_daily"} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE node_id=?`, f.id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("queued report resurrected %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := f.service.Latest(ctx, f.id); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("queued report exposed latest: %v", err)
	}
	// A request authenticated after the reset can claim the fresh identity;
	// neither the rejected report nor its pending counters may produce ACKs.
	f.report.Traffic = nil
	f.report.ProbeResults = nil
	f.report.Runtime.ProbeTargets = nil
	f.report.System.Hostname = "fresh-host"
	response, err := f.service.AcceptToken(ctx, coalesceToken, f.report)
	if err != nil || response.AcceptedSeq != 1 || len(response.TrafficAcks) != 0 {
		t.Fatalf("fresh report response=%+v err=%v", response, err)
	}
	if err := f.service.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"probe_latest", "probe_hourly", "traffic_hourly", "traffic_daily"} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE node_id=?`, f.id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("pending old identity resurrected %s after fresh report/drain: count=%d err=%v", table, count, err)
		}
	}
}

func TestCoalesceCanceledSameNodeWaiterDoesNotBlockShutdown(t *testing.T) {
	f := newCoalesceFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Occupy writer admission so a durable report holds the node gate.
	releaseWrite, err := f.service.beginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	defer release.Do(releaseWrite)
	f.report.Seq++
	f.report.System.Hostname = "durable change"
	slowCtx, cancelSlow := context.WithCancel(ctx)
	defer cancelSlow()
	slow := make(chan error, 1)
	go func() {
		_, err := f.service.accept(slowCtx, f.id, nil, 0, f.report, false)
		slow <- err
	}()
	for len(f.service.writerQueue) != 2 {
		select {
		case <-ctx.Done():
			t.Fatal("durable report did not hold the node gate")
		case <-time.After(time.Millisecond):
		}
	}
	latestCtx, cancelLatest := context.WithTimeout(ctx, time.Second)
	defer cancelLatest()
	if latest, err := f.service.Latest(latestCtx, f.id); err != nil || latest.Seq != 1 {
		t.Fatalf("slow same-node writer blocked Latest: latest=%+v err=%v", latest, err)
	}
	waiterCtx, cancelWaiter := context.WithCancel(ctx)
	defer cancelWaiter()
	waiter := make(chan error, 1)
	go func() {
		_, err := f.service.accept(waiterCtx, f.id, nil, 0, f.report, false)
		waiter <- err
	}()
	state := f.service.acquireExistingState(f.id)
	defer f.service.releaseState(state)
	for {
		f.service.mu.Lock()
		refs := state.refs
		f.service.mu.Unlock()
		if refs == 3 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("same-node waiter did not reach gate")
		case <-time.After(time.Millisecond):
		}
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- f.service.Shutdown(ctx) }()
	cancelWaiter()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled same-node waiter remained blocked by durable report")
	}
	// The gate is still occupied when the cancelled waiter leaves the drain.
	f.service.lifecycleMu.Lock()
	inflight := f.service.inflight
	f.service.lifecycleMu.Unlock()
	if inflight != 2 {
		t.Fatalf("cancelled waiter still counted for shutdown: inflight=%d", inflight)
	}
	cancelSlow()
	select {
	case err := <-slow:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("slow report returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow writer did not cancel")
	}
	release.Do(releaseWrite)
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled report blocked shutdown")
	}
}

func TestTypedRuntimeSelectionsStayCanonicalWithoutAnotherDurableWrite(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Runtime.Interfaces = nil
	f.report.Runtime.DiskPaths = nil
	f.report.Runtime.ProbeTargets = nil
	if _, err := f.service.accept(context.Background(), f.id, nil, 0, f.report, false); err != nil {
		t.Fatal(err)
	}
	latest, err := f.service.Latest(context.Background(), f.id)
	if err != nil || latest.Runtime.Interfaces == nil || latest.Runtime.DiskPaths == nil || latest.Runtime.ProbeTargets == nil {
		t.Fatalf("latest runtime=%+v err=%v", latest.Runtime, err)
	}
	observed, err := f.nodes.GetRuntime(context.Background(), f.id)
	if err != nil || observed == nil || observed.Interfaces == nil || observed.DiskPaths == nil || observed.ProbeTargets == nil {
		t.Fatalf("observed runtime=%+v err=%v", observed, err)
	}
	if f.writes(t) != 1 {
		t.Fatal("equivalent empty runtime selections caused another durable write")
	}
}
