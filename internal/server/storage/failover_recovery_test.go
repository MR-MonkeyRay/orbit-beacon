package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFailoverRecoveryDiscardsDivergedOldTailAndPreservesNewDeletes(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('common','kept')", "INSERT INTO settings VALUES('deleted-on-standby','old')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	selected, err := position(ctx, d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('old-tail','discard')")
	if err = d.switchTo(ctx, "standby", selected); err != nil {
		t.Fatal(err)
	}
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('new-branch','kept')", "DELETE FROM settings WHERE `key`='deleted-on-standby'")
	if err = d.synchronize(ctx); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key  string
		want int
	}{{"common", 1}, {"old-tail", 0}, {"new-branch", 1}, {"deleted-on-standby", 0}} {
		var count int
		if err = d.target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE `key`=?", item.key).Scan(&count); err != nil || count != item.want {
			t.Fatalf("%s rows=%d want=%d err=%v", item.key, count, item.want, err)
		}
	}
	if !m.Ready || m.Rebuild != nil {
		t.Fatal("completed target did not become a complete copy")
	}
}

func TestFailoverRecoveryUsesExactAncestorPrefixIncrementally(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('common','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	selected, err := position(ctx, d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.switchTo(ctx, "standby", selected); err != nil {
		t.Fatal(err)
	}
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('new-branch','kept')")
	if err = d.synchronize(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.Ready || m.Checkpoint != nil {
		t.Fatal("exact ancestor was rebuilt instead of incrementally backfilled")
	}
	active, err := position(ctx, d.source.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	inactive, err := position(ctx, d.target.current.reader)
	if err != nil || active != inactive {
		t.Fatal("incremental recovery did not reach the same head", active, inactive, err)
	}
}

func TestFailoverDeadlineAndClockLockSurviveControlReopen(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	boot, nanos, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	wall := time.Now().UTC()
	m.Freeze = &freezePoint{Boot: boot, Nanos: nanos, Wall: wall}
	m.FailureSince = wall
	d.control.clock = func() (string, int64, time.Time, error) {
		return boot, nanos + int64(24*time.Hour), wall.Add(24 * time.Hour), nil
	}
	if err = d.checkManual(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.Manual {
		t.Fatal("24h recovery deadline did not lock")
	}
	r, err := openControl(ctx, d.control.dataDir, false)
	// failoverPair 的 control 目录取实际文件路径，不另建或重置 control。
	if err != nil {
		t.Fatal(err)
	}
	defer r.control.Close()
	if !r.record.Domains[MainDomain].Manual {
		t.Fatal("manual lock was not durable")
	}
	d.control.clock = func() (string, int64, time.Time, error) { return boot, nanos, wall.Add(-time.Hour), nil }
	if err = d.checkManual(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.Manual {
		t.Fatal("wall rollback cleared manual lock")
	}
}

func TestFailoverBusyGateDoesNotFreezeHealthyDomain(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	d.source.writeGate <- struct{}{}
	err := d.barrier(ctx)
	<-d.source.writeGate
	if !errors.Is(err, errReplicaProbeBusy) {
		t.Fatal("healthy busy gate was not distinguished", err)
	}
	if m.Freeze != nil || !d.source.Ready() {
		t.Fatal("local write contention froze healthy storage")
	}
}
