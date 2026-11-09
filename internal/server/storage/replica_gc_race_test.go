package storage

import (
	"context"
	"database/sql/driver"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func replicaReviewControl(t *testing.T, source, target *Database, ready bool, state string) *replication {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := openControl(ctx, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.control.Close() })
	d := source.replica
	d.control = r
	d.domain = source.domain
	d.target = target
	r.domains[d.domain] = d
	r.record = controlRecord{Cluster: "review-cluster", Binding: "[]", Domains: map[Domain]*memberRecord{d.domain: {Resource: d.resource, Primary: "0", Standby: "1", Epoch: d.epoch, Active: "primary", Ready: ready, State: state}}}
	if err = r.persistLocked(ctx); err != nil {
		t.Fatal(err)
	}
	d.revoke = func(ctx context.Context) error { return r.revoke(ctx, d.domain, "rebuild_required") }
	return r
}
func TestReplicaGCRebuildSurvivesInFlightGap(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := replicaReviewControl(t, source, target, true, "healthy")
	for _, key := range []string{"a", "b", "c"} {
		replicaWrite(t, source, "INSERT INTO settings VALUES('"+key+"','kept')")
	}
	if _, err := target.current.writer.ExecContext(ctx, "UPDATE replica_state SET ready=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	reached, resume := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	if err := observeReplicaDatabase(ctx, source, nil, func(call context.Context, q string, _ []driver.NamedValue) {
		if strings.Contains(q, "replica_identity") && strings.Contains(q, "resource_uuid") && blocked.CompareAndSwap(false, true) {
			close(reached)
			select {
			case <-resume:
			case <-call.Done():
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { source.replica.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up did not consume the healthy member snapshot")
	}
	if err := source.replica.prune(ctx, time.Now().Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	state, ready := r.record.Domains[MainDomain].State, r.record.Domains[MainDomain].Ready
	r.mu.Unlock()
	if state != "rebuild_required" || ready {
		t.Fatalf("GC did not revoke before deleting: %s %v", state, ready)
	}
	close(resume)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		state, ready = r.record.Domains[MainDomain].State, r.record.Domains[MainDomain].Ready
		r.mu.Unlock()
		if state == "manual_required" {
			t.Fatal("stale catch-up failure overwrote durable rebuild_required")
		}
		if state == "healthy" && ready {
			var count int
			if err := target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings").Scan(&count); err != nil || count != 3 {
				t.Fatal("rebuild lost source rows", count, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GC gap never restarted seeding: %s %v", state, ready)
}
func TestReplicaIdentityFailureStillStopsAfterRebuild(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := replicaReviewControl(t, source, target, false, "rebuild_required")
	if _, err := target.current.writer.ExecContext(ctx, "INSERT INTO settings VALUES('kept','foreign'); UPDATE replica_identity SET resource_uuid='foreign' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { source.replica.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		state, ready := r.record.Domains[MainDomain].State, r.record.Domains[MainDomain].Ready
		r.mu.Unlock()
		if state == "manual_required" && !ready {
			var value string
			if err := target.current.reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='kept'").Scan(&value); err != nil || value != "foreign" {
				t.Fatal("foreign target was cleared", value, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("identity failure was hidden by rebuild state")
}
