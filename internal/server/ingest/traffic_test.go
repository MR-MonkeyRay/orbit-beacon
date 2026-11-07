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

func trafficEntry(epoch string, period time.Time, rx, tx string) v1.Traffic {
	return v1.Traffic{Epoch: epoch, PeriodStart: period, RxBytes: v1.DecimalUint64(rx), TxBytes: v1.DecimalUint64(tx)}
}

func trafficAccept(t *testing.T, f *coalesceFixture) (v1.ReportResponse, error) {
	t.Helper()
	return f.service.accept(context.Background(), f.id, nil, 0, f.report)
}

func TestTrafficCoalescesAcksAfterCommitAndMaxUpsert(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	period := f.now.Truncate(time.Hour)
	f.report.Traffic = []v1.Traffic{trafficEntry("a", period, "10", "20")}
	response, err := trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 0 || f.writes(t) != 1 {
		t.Fatalf("uncommitted response=%+v err=%v writes=%d", response, err, f.writes(t))
	}
	f.now = f.now.Add(30 * time.Second)
	f.report.Seq++
	f.report.Traffic[0].RxBytes = "15"
	response, err = trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].RxBytes != "15" {
		t.Fatalf("committed response=%+v err=%v", response, err)
	}
	// 较低值的重发无需写入即可确认实际提交的最大值。
	f.report.Seq++
	f.report.Traffic[0].RxBytes = "1"
	f.report.Traffic[0].TxBytes = "25"
	response, err = trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].RxBytes != "15" || response.TrafficAcks[0].TxBytes != "20" {
		t.Fatalf("lagged ack=%+v err=%v", response, err)
	}
	f.now = f.now.Add(30 * time.Second)
	f.report.Seq++
	response, err = trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].RxBytes != "15" || response.TrafficAcks[0].TxBytes != "25" {
		t.Fatalf("max ack=%+v err=%v", response, err)
	}
	// 重启后没有 ack 缓存；必须由 RETURNING 恢复现有最大值。
	f.service = NewService(f.db, Options{Now: func() time.Time { return f.now }})
	f.report.Seq++
	response, err = trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].RxBytes != "15" || response.TrafficAcks[0].TxBytes != "25" {
		t.Fatalf("restart ack=%+v err=%v", response, err)
	}
	var count, rx, tx int64
	if err := f.db.QueryRow(`SELECT count(*),sum(rx_bytes),sum(tx_bytes) FROM traffic_hourly WHERE node_id=?`, f.id).Scan(&count, &rx, &tx); err != nil || count != 1 || rx != 15 || tx != 25 {
		t.Fatalf("count=%d rx=%d tx=%d err=%v", count, rx, tx, err)
	}
}

func TestTrafficPersistFailureRetainsPendingAndNeverAcks(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(31 * time.Second)
	f.report.Seq++
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "10", "20")}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_traffic BEFORE INSERT ON traffic_hourly BEGIN SELECT RAISE(ABORT,'traffic write failed'); END`); err != nil {
		t.Fatal(err)
	}
	response, err := trafficAccept(t, f)
	if err == nil || len(response.TrafficAcks) != 0 {
		t.Fatalf("failed persist ack=%+v err=%v", response, err)
	}
	state := f.service.acquireExistingState(f.id)
	defer f.service.releaseState(state)
	state.mu.Lock()
	pending := len(state.traffic.pending)
	committed := len(state.traffic.committed)
	state.mu.Unlock()
	if pending != 1 || committed != 0 || f.writes(t) != 1 {
		t.Fatalf("pending=%d committed=%d writes=%d", pending, committed, f.writes(t))
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_traffic`); err != nil {
		t.Fatal(err)
	}
	// 报告中没有流量时重试，仍会刷写保留的缓冲区。
	f.report.Traffic = nil
	f.report.Seq++
	if _, err := trafficAccept(t, f); err != nil {
		t.Fatal(err)
	}
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "10", "20")}
	f.report.Seq++
	response, err = trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 1 {
		t.Fatalf("retry=%+v err=%v", response, err)
	}
}

func TestTrafficSkipsFutureAndAcknowledgesExpired(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(31 * time.Second)
	f.report.Seq++
	expired := trafficEntry("a", f.now.Add(-36*24*time.Hour).Truncate(time.Hour), "10", "20")
	future := trafficEntry("b", f.now.Add(2*time.Hour).Truncate(time.Hour), "30", "40")
	f.report.Traffic = []v1.Traffic{expired, future}
	response, err := trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0] != expired {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM traffic_hourly`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestTrafficShutdownFlushAndIdentityFences(t *testing.T) {
	for _, mutation := range []string{"none", "delete", "revoke", "disable", "credential_epoch", "generation", "aba"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixture(t, 30*time.Second)
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "10", "20")}
			if response, err := trafficAccept(t, f); err != nil || len(response.TrafficAcks) != 0 {
				t.Fatalf("response=%+v err=%v", response, err)
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
			case "aba":
				_, err = f.db.Exec(`UPDATE nodes SET run_generation='0',report_session_id='' WHERE id=?`, f.id)
				if err == nil {
					f.report.Traffic = nil
					f.report.Seq++
					_, err = trafficAccept(t, f)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM traffic_hourly`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mutation == "none" {
				want = 1
			}
			if count != want {
				t.Fatalf("fenced %s: count=%d want=%d", mutation, count, want)
			}
			if mutation == "delete" {
				if _, err := trafficAccept(t, f); !errors.Is(err, ErrServiceClosed) && !errors.Is(err, nodes.ErrUnauthenticated) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTrafficPendingAndCommittedAreBounded(t *testing.T) {
	state := &nodeState{}
	credential := sha256.Sum256([]byte("credential"))
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for n := 0; n < 100; n++ {
		report := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 1, SessionID: "session", Traffic: []v1.Traffic{trafficEntry(fmt.Sprintf("%x", n), now, "1", "2")}}
		state.bufferTraffic(credential[:], 0, report, 1, "session", now)
	}
	if len(state.traffic.pending) != trafficBufferLimit {
		t.Fatalf("pending=%d", len(state.traffic.pending))
	}
	committed := make(map[trafficKey]trafficCounters)
	for n := 0; n < 100; n++ {
		committed[trafficKey{fmt.Sprintf("%x", n), timekey.Format(now)}] = trafficCounters{1, 2}
	}
	state.commitTraffic(committed, now)
	if len(state.traffic.committed) > trafficBufferLimit || len(state.traffic.pending) > trafficBufferLimit {
		t.Fatalf("unbounded traffic buffer")
	}
}

func TestTrafficDefensiveValidation(t *testing.T) {
	hour := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	valid := trafficEntry("a0", hour, "1", "2")
	if err := validateTraffic([]v1.Traffic{valid}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*v1.Traffic){
		func(e *v1.Traffic) { e.Epoch = "A" }, func(e *v1.Traffic) { e.Epoch = "" },
		func(e *v1.Traffic) { e.PeriodStart = e.PeriodStart.Add(time.Second) },
		func(e *v1.Traffic) { e.PeriodStart = e.PeriodStart.In(time.FixedZone("offset", 3600)) },
		func(e *v1.Traffic) { e.RxBytes = "9223372036854775808" }, func(e *v1.Traffic) { e.TxBytes = "18446744073709551615" },
	} {
		entry := valid
		mutate(&entry)
		if err := validateTraffic([]v1.Traffic{entry}); err == nil {
			t.Fatalf("accepted %+v", entry)
		}
	}
	if err := validateTraffic([]v1.Traffic{valid, valid}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := validateTraffic(make([]v1.Traffic, 25)); err == nil {
		t.Fatal("oversized accepted")
	}
}

func TestTrafficCommitFailureRetainsPending(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(31 * time.Second)
	f.report.Seq++
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "10", "20")}
	// INSERT 成功，但延迟的外键违例使 COMMIT 失败。
	if _, err := f.db.Exec(`CREATE TABLE traffic_commit_guard(node_id TEXT REFERENCES nodes(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_traffic_commit AFTER INSERT ON traffic_hourly BEGIN INSERT INTO traffic_commit_guard VALUES('missing-node'); END`); err != nil {
		t.Fatal(err)
	}
	response, err := trafficAccept(t, f)
	if err == nil || len(response.TrafficAcks) != 0 {
		t.Fatalf("commit failure response=%+v err=%v", response, err)
	}
	state := f.service.acquireExistingState(f.id)
	defer f.service.releaseState(state)
	state.mu.Lock()
	pending := len(state.traffic.pending)
	committed := len(state.traffic.committed)
	state.mu.Unlock()
	if pending != 1 || committed != 0 || f.writes(t) != 1 {
		t.Fatalf("pending=%d committed=%d writes=%d", pending, committed, f.writes(t))
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_traffic_commit`); err != nil {
		t.Fatal(err)
	}
	// 即使没有新报告，Shutdown 也会重试保留的流量。
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rx, tx int64
	if err := f.db.QueryRow(`SELECT rx_bytes,tx_bytes FROM traffic_hourly WHERE node_id=?`, f.id).Scan(&rx, &tx); err != nil || rx != 10 || tx != 20 {
		t.Fatalf("drained counters=(%d,%d), err=%v", rx, tx, err)
	}
}

func TestTrafficFailedShutdownFlushCanRetry(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "10", "20")}
	if _, err := trafficAccept(t, f); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_shutdown_traffic BEFORE INSERT ON traffic_hourly BEGIN SELECT RAISE(ABORT,'traffic write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Shutdown(context.Background()); err == nil {
		t.Fatal("shutdown ignored traffic failure")
	}
	state := f.service.acquireExistingState(f.id)
	defer f.service.releaseState(state)
	state.mu.Lock()
	pending := len(state.traffic.pending)
	committed := len(state.traffic.committed)
	state.mu.Unlock()
	if pending != 1 || committed != 0 {
		t.Fatalf("pending=%d committed=%d", pending, committed)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_shutdown_traffic`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM traffic_hourly WHERE node_id=?`, f.id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestTrafficExpiredPendingReleasesCapacity(t *testing.T) {
	state := &nodeState{}
	credential := sha256.Sum256([]byte("credential"))
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for n := 0; n < trafficBufferLimit; n++ {
		report := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 1, SessionID: "session", Traffic: []v1.Traffic{trafficEntry(fmt.Sprintf("%x", n), now, "1", "2")}}
		state.bufferTraffic(credential[:], 0, report, 1, "session", now)
	}
	now = now.Add(36 * 24 * time.Hour)
	report := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 1, SessionID: "session", Traffic: []v1.Traffic{trafficEntry("ff", now, "3", "4")}}
	state.bufferTraffic(credential[:], 0, report, 1, "session", now)
	if len(state.traffic.pending) != 1 {
		t.Fatalf("expired buckets occupy pending capacity: %d", len(state.traffic.pending))
	}
	if _, ok := state.traffic.pending[trafficKey{"ff", timekey.Format(now)}]; !ok {
		t.Fatal("new traffic was dropped after expiry")
	}
}

func TestTrafficShutdownAttemptsEveryNodeAfterFailure(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "second-traffic"})
	if err != nil {
		t.Fatal(err)
	}
	credential := sha256.Sum256([]byte("second-traffic-credential"))
	if _, err := f.db.Exec(`UPDATE nodes SET token_hash=?,status='unknown' WHERE id=?`, credential[:], node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.accept(context.Background(), node.ID, nil, 0, f.report); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "13", "20")}
	if _, err := trafficAccept(t, f); err != nil {
		t.Fatal(err)
	}
	second := f.report
	second.Traffic = []v1.Traffic{trafficEntry("b", f.now.Truncate(time.Hour), "10", "20")}
	if _, err := f.service.accept(context.Background(), node.ID, nil, 0, second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_one_node_traffic BEFORE INSERT ON traffic_hourly WHEN NEW.rx_bytes=13 BEGIN SELECT RAISE(ABORT,'one node failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Shutdown(context.Background()); err == nil {
		t.Fatal("shutdown ignored failed node")
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM traffic_hourly WHERE node_id=?`, node.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("healthy node not drained: count=%d err=%v", count, err)
	}
}

func TestTrafficFailedProcessAdvanceDrainsUnderObservedFence(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.RunGeneration++
	f.report.SessionID = "next-process"
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "10", "20")}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_process_traffic BEFORE INSERT ON traffic_hourly BEGIN SELECT RAISE(ABORT,'traffic write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := trafficAccept(t, f); err == nil {
		t.Fatal("process advance unexpectedly committed")
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_process_traffic`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM traffic_hourly WHERE node_id=?`, f.id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending process traffic lost: count=%d err=%v", count, err)
	}
	var generation string
	if err := f.db.QueryRow(`SELECT run_generation FROM nodes WHERE id=?`, f.id).Scan(&generation); err != nil || generation != "1" {
		t.Fatalf("drain advanced report fence: generation=%q err=%v", generation, err)
	}
}

func TestTrafficStagesRestartWithoutReplacingAcceptedCounters(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	credential := sha256.Sum256([]byte("restart-credential"))
	oldKey := trafficKey{"a", timekey.Format(now)}
	newKey := trafficKey{"b", timekey.Format(now)}
	state := &nodeState{}
	old := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 1, SessionID: "old", Traffic: []v1.Traffic{trafficEntry("a", now, "100", "200")}}
	state.bufferTraffic(credential[:], 0, old, 1, "old", now)
	state.commitTraffic(map[trafficKey]trafficCounters{oldKey: {100, 200}}, now)
	old.Traffic[0] = trafficEntry("a", now, "150", "250")
	state.bufferTraffic(credential[:], 0, old, 1, "old", now)
	incoming := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 2, SessionID: "new", Traffic: []v1.Traffic{trafficEntry("b", now, "25", "35")}}
	state.stageTraffic(credential[:], 0, incoming, 1, "old", now)
	if state.traffic.generation != 1 || state.traffic.pending[oldKey].rx != 150 || state.traffic.committed[oldKey].rx != 100 {
		t.Fatal("incoming run replaced accepted counters before commit")
	}
	if state.stagedTraffic == nil || state.stagedTraffic.pending[newKey].rx != 25 {
		t.Fatal("incoming run was not staged separately")
	}
	if acks := state.trafficAcks(incoming.Traffic, now); len(acks) != 0 {
		t.Fatalf("staged traffic acknowledged before commit: %+v", acks)
	}
	// 在失败事务之后观测到相同的持久化围栏时，两者都保留。
	state.fenceTraffic(credential[:], 0, 1, "old")
	if state.traffic.pending[oldKey].rx != 150 || state.stagedTraffic == nil {
		t.Fatal("rollback lost accepted counters")
	}
	state.commitTraffic(map[trafficKey]trafficCounters{newKey: {25, 35}}, now)
	if state.stagedTraffic != nil || state.traffic.generation != 2 || len(state.traffic.pending) != 0 {
		t.Fatal("commit did not promote incoming run")
	}
	if acks := state.trafficAcks(incoming.Traffic, now); len(acks) != 1 || acks[0].RxBytes != "25" {
		t.Fatalf("committed traffic not acknowledged: %+v", acks)
	}
}

func TestTrafficStagedRestartStillFencesIdentityChanges(t *testing.T) {
	for _, mutation := range []string{"credential", "generation", "session"} {
		t.Run(mutation, func(t *testing.T) {
			now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
			credential := sha256.Sum256([]byte("restart-credential"))
			state := &nodeState{}
			old := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 1, SessionID: "old", Traffic: []v1.Traffic{trafficEntry("a", now, "150", "250")}}
			incoming := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 2, SessionID: "new", Traffic: []v1.Traffic{trafficEntry("b", now, "25", "35")}}
			state.bufferTraffic(credential[:], 0, old, 1, "old", now)
			state.stageTraffic(credential[:], 0, incoming, 1, "old", now)
			generation, session := uint64(1), "old"
			switch mutation {
			case "credential":
				credential = sha256.Sum256([]byte("rotated"))
			case "generation":
				generation = 3
			case "session":
				session = "replacement"
			}
			state.fenceTraffic(credential[:], 0, generation, session)
			if len(state.traffic.pending) != 0 || state.stagedTraffic != nil {
				t.Fatal("old traffic survived durable identity mismatch")
			}
		})
	}
}

func newTrafficRestartFixture(t *testing.T) *coalesceFixture {
	t.Helper()
	f := newCoalesceFixture(t, 30*time.Second)
	f.now = f.now.Add(31 * time.Second)
	f.report.Seq++
	f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "100", "200")}
	if _, err := trafficAccept(t, f); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	f.report.Traffic[0] = trafficEntry("a", f.now.Truncate(time.Hour), "150", "250")
	if response, err := trafficAccept(t, f); err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].RxBytes != "100" {
		t.Fatalf("coalesced old traffic response=%+v err=%v", response, err)
	}
	return f
}

func TestTrafficRestartFlushesAcceptedCountersAtomically(t *testing.T) {
	for _, failure := range []string{"none", "new_traffic", "node_update", "commit"} {
		t.Run(failure, func(t *testing.T) {
			f := newTrafficRestartFixture(t)
			baseline := f.writes(t)
			f.report.RunGeneration++
			f.report.SessionID = "restarted"
			f.report.Seq = 1
			f.report.Traffic = []v1.Traffic{trafficEntry("b", f.now.Truncate(time.Hour), "25", "35")}
			var trigger string
			switch failure {
			case "new_traffic":
				trigger = `CREATE TRIGGER fail_restart BEFORE INSERT ON traffic_hourly WHEN NEW.epoch='b' BEGIN SELECT RAISE(ABORT,'new traffic failed'); END`
			case "node_update":
				trigger = `CREATE TRIGGER fail_restart BEFORE UPDATE OF run_generation ON nodes WHEN NEW.run_generation='2' BEGIN SELECT RAISE(ABORT,'node update failed'); END`
			case "commit":
				trigger = `CREATE TABLE restart_commit_guard(node_id TEXT REFERENCES nodes(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_restart AFTER INSERT ON traffic_hourly WHEN NEW.epoch='b' BEGIN INSERT INTO restart_commit_guard VALUES('missing-node'); END`
			}
			if trigger != "" {
				if _, err := f.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			response, err := trafficAccept(t, f)
			if failure != "none" {
				if err == nil || len(response.TrafficAcks) != 0 {
					t.Fatalf("failed restart acknowledged: response=%+v err=%v", response, err)
				}
				var rx int64
				if err := f.db.QueryRow(`SELECT rx_bytes FROM traffic_hourly WHERE node_id=? AND epoch='a'`, f.id).Scan(&rx); err != nil || rx != 100 {
					t.Fatalf("rollback changed old durable counters: rx=%d err=%v", rx, err)
				}
				state := f.service.acquireExistingState(f.id)
				state.mu.Lock()
				key := trafficKey{"a", timekey.Format(f.now.Truncate(time.Hour))}
				retained := state.traffic.generation == 1 && state.traffic.pending[key].rx == 150 && state.stagedTraffic != nil
				state.mu.Unlock()
				f.service.releaseState(state)
				if !retained || f.writes(t) != baseline {
					t.Fatal("rollback replaced old buffer or advanced report fence")
				}
				if _, err := f.db.Exec(`DROP TRIGGER fail_restart`); err != nil {
					t.Fatal(err)
				}
				response, err = trafficAccept(t, f)
			}
			if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].Epoch != "b" || response.TrafficAcks[0].RxBytes != "25" {
				t.Fatalf("restart response=%+v err=%v", response, err)
			}
			var count, rx, tx int64
			if err := f.db.QueryRow(`SELECT count(*),sum(rx_bytes),sum(tx_bytes) FROM traffic_hourly WHERE node_id=?`, f.id).Scan(&count, &rx, &tx); err != nil || count != 2 || rx != 175 || tx != 285 {
				t.Fatalf("accepted restart traffic lost: count=%d rx=%d tx=%d err=%v", count, rx, tx, err)
			}
			var generation, session string
			if err := f.db.QueryRow(`SELECT run_generation,report_session_id FROM nodes WHERE id=?`, f.id).Scan(&generation, &session); err != nil || generation != "2" || session != "restarted" {
				t.Fatalf("fence=%s/%s err=%v", generation, session, err)
			}
		})
	}
}

func TestTrafficRestartDiscardsPendingAcrossCredentialOrDurableFenceChanges(t *testing.T) {
	for _, mutation := range []string{"credential", "credential_epoch", "generation", "session"} {
		t.Run(mutation, func(t *testing.T) {
			f := newTrafficRestartFixture(t)
			// 在外部变更之前，于一次失败的尝试期间暂存新运行。
			f.report.RunGeneration = 2
			f.report.SessionID = "staged"
			f.report.Seq = 1
			f.report.Traffic = []v1.Traffic{trafficEntry("b", f.now.Truncate(time.Hour), "25", "35")}
			if _, err := f.db.Exec(`CREATE TRIGGER fail_staged BEFORE INSERT ON traffic_hourly WHEN NEW.epoch='b' BEGIN SELECT RAISE(ABORT,'staged failed'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := trafficAccept(t, f); err == nil {
				t.Fatal("staged report unexpectedly committed")
			}
			if _, err := f.db.Exec(`DROP TRIGGER fail_staged`); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "credential":
				rotated := sha256.Sum256([]byte("rotated-traffic-credential"))
				if _, err := f.db.Exec(`UPDATE nodes SET token_hash=? WHERE id=?`, rotated[:], f.id); err != nil {
					t.Fatal(err)
				}
			case "credential_epoch":
				if _, err := f.db.Exec(`UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?`, f.id); err != nil {
					t.Fatal(err)
				}
			case "generation":
				if _, err := f.db.Exec(`UPDATE nodes SET run_generation='2',report_session_id='external' WHERE id=?`, f.id); err != nil {
					t.Fatal(err)
				}
			case "session":
				if _, err := f.db.Exec(`UPDATE nodes SET report_session_id='external' WHERE id=?`, f.id); err != nil {
					t.Fatal(err)
				}
			}
			f.report.RunGeneration = 3
			f.report.SessionID = "current"
			f.report.Seq = 1
			f.report.Traffic = []v1.Traffic{trafficEntry("c", f.now.Truncate(time.Hour), "30", "40")}
			response, err := trafficAccept(t, f)
			if err != nil || len(response.TrafficAcks) != 1 || response.TrafficAcks[0].Epoch != "c" {
				t.Fatalf("new identity response=%+v err=%v", response, err)
			}
			var rx int64
			if err := f.db.QueryRow(`SELECT rx_bytes FROM traffic_hourly WHERE node_id=? AND epoch='a'`, f.id).Scan(&rx); err != nil || rx != 100 {
				t.Fatalf("invalidated old pending carried over: rx=%d err=%v", rx, err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM traffic_hourly WHERE node_id=? AND epoch='b'`, f.id).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalidated staged counters carried over: count=%d err=%v", count, err)
			}
		})
	}
}

func TestTrafficRestartShutdownRetriesBothBuffers(t *testing.T) {
	f := newTrafficRestartFixture(t)
	f.report.RunGeneration = 2
	f.report.SessionID = "staged"
	f.report.Seq = 1
	f.report.Traffic = []v1.Traffic{trafficEntry("b", f.now.Truncate(time.Hour), "25", "35")}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_restart_shutdown BEFORE INSERT ON traffic_hourly WHEN NEW.epoch='b' BEGIN SELECT RAISE(ABORT,'new traffic failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := trafficAccept(t, f); err == nil {
		t.Fatal("restart unexpectedly committed")
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_restart_shutdown`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count, rx, tx int64
	if err := f.db.QueryRow(`SELECT count(*),sum(rx_bytes),sum(tx_bytes) FROM traffic_hourly WHERE node_id=?`, f.id).Scan(&count, &rx, &tx); err != nil || count != 2 || rx != 175 || tx != 285 {
		t.Fatalf("shutdown lost restart counters: count=%d rx=%d tx=%d err=%v", count, rx, tx, err)
	}
}

func TestTrafficCredentialRotationAfterInitialReadCannotCarryPending(t *testing.T) {
	f := newTrafficRestartFixture(t)
	f.report.RunGeneration = 2
	f.report.SessionID = "new-credential"
	f.report.Seq = 1
	f.report.Traffic = []v1.Traffic{trafficEntry("b", f.now.Truncate(time.Hour), "25", "35")}
	rotated := sha256.Sum256([]byte("rotated-during-traffic-read"))
	var rotatedOnce bool
	// accept 在首次节点身份读取之后、持久化事务之前采样该时钟。
	// 事务围栏必须看到这次替换。
	f.service.now = func() time.Time {
		if !rotatedOnce {
			rotatedOnce = true
			if _, err := f.db.Exec(`UPDATE nodes SET token_hash=? WHERE id=?`, rotated[:], f.id); err != nil {
				t.Fatal(err)
			}
		}
		return f.now
	}
	response, err := trafficAccept(t, f)
	if !errors.Is(err, nodes.ErrUnauthenticated) || len(response.TrafficAcks) != 0 {
		t.Fatalf("queued old-credential response=%+v err=%v", response, err)
	}
	response, err = trafficAccept(t, f)
	if err != nil || len(response.TrafficAcks) != 1 {
		t.Fatalf("fresh identity response=%+v err=%v", response, err)
	}
	var rx int64
	if err := f.db.QueryRow(`SELECT rx_bytes FROM traffic_hourly WHERE node_id=? AND epoch='a'`, f.id).Scan(&rx); err != nil || rx != 100 {
		t.Fatalf("old credential carried pending after read: rx=%d err=%v", rx, err)
	}
}

func TestTrafficStagesRetryWhenOnlyIncomingBufferSurvivesFence(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	credential := sha256.Sum256([]byte("credential"))
	state := &nodeState{}
	old := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 1, SessionID: "old", Traffic: []v1.Traffic{trafficEntry("a", now, "150", "250")}}
	incoming := v1.Report{Runtime: v1.DefaultRuntimeSettings(), RunGeneration: 2, SessionID: "new", Traffic: []v1.Traffic{trafficEntry("b", now, "25", "35")}}
	state.bufferTraffic(credential[:], 0, old, 1, "old", now)
	state.stageTraffic(credential[:], 0, incoming, 1, "old", now)
	// 模拟一个新到来的缓冲区，其准入时观测到的身份比已接受运行更新，
	// 然后单独清除旧运行的数据。
	state.stagedTraffic.observedSession = "external"
	state.fenceTraffic(credential[:], 0, 1, "external")
	incoming.Traffic = nil
	state.stageTraffic(credential[:], 0, incoming, 1, "external", now)
	key := trafficKey{"b", timekey.Format(now)}
	if state.traffic.pending != nil || state.stagedTraffic == nil || state.stagedTraffic.pending[key].rx != 25 {
		t.Fatal("retry discarded fenced incoming pending counters")
	}
}
