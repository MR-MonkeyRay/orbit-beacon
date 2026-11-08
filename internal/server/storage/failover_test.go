package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
)

func TestHAFailureFreezesAdmissionAndCancelsGeneration(t *testing.T) {
	source, _ := replicaPair(t, MainDomain)
	lease, err := source.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	source.current.dialect = MySQL
	lease.failure(io.EOF)
	next, err := source.Acquire(context.Background(), Reader)
	if next != nil {
		next.Close()
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed generation admitted a request: %v", err)
	}
	if lease.Context().Err() == nil {
		t.Fatal("in-flight generation was not cancelled")
	}
}

func failoverPair(t *testing.T) (*replicaDomain, *memberRecord) {
	t.Helper()
	source, target := replicaPair(t, MainDomain)
	if _, err := target.current.writer.ExecContext(context.Background(), "UPDATE replica_state SET ready=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := openControl(context.Background(), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.control.Close() })
	r.cfg.Primary = config.DatabaseEndpoint{Driver: "sqlite"}
	r.cfg.Standby = &config.DatabaseEndpoint{Driver: "sqlite"}
	r.cfg.Failover = config.FailoverConfig{FailureTimeout: 30 * time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: 60 * time.Second, ManualAfter: 24 * time.Hour}
	m := &memberRecord{Resource: "resource", Primary: "0", Standby: "1", Epoch: "epoch", Active: "primary", State: "healthy", Ready: true, PrimaryRegistered: true, StandbyRegistered: true, Generation: 1}
	r.record = controlRecord{Cluster: "cluster", Binding: "[]", Domains: map[Domain]*memberRecord{MainDomain: m}}
	d := source.replica
	d.control, d.domain = r, MainDomain
	r.domains[MainDomain] = d
	if err := r.persistLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, m
}

func TestFailoverProofRejectsOldUnknownAndCrossBootBarrier(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('proof','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	// 重启后源不可达，没有本进程 possible-commit 证据，只能依赖候选时间屏障。
	d.source = unavailableDatabase(MainDomain)
	d.source.replica = d
	boot, stamp, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	m.Freeze = &freezePoint{Boot: boot, Nanos: stamp, Wall: time.Now().UTC()}
	m.FailureSince = m.Freeze.Wall
	cases := []struct {
		name, boot string
		barrier    int64
		ready      bool
		want       bool
	}{
		{"within", boot, stamp - int64(10*time.Second), true, true},
		{"old", boot, stamp - int64(10*time.Second) - 1, true, false},
		{"cross_boot", "other-boot", stamp, true, false},
		{"future", boot, stamp + 1, true, false},
		{"half_rebuild", boot, stamp, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.Ready = c.ready
			if _, err := d.target.current.writer.ExecContext(ctx, "UPDATE replica_transactions SET boot_id=?,barrier_ns=?", c.boot, c.barrier); err != nil {
				t.Fatal(err)
			}
			_, err := d.promotionCheckpoint(ctx, *m)
			if (err == nil) != c.want {
				t.Fatalf("proof result=%v want valid=%v", err, c.want)
			}
		})
	}
}

func TestFailoverSwitchPublishesCandidateLeaseAfterDrain(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('proof','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := d.source.Acquire(ctx, Reader)
	if err != nil {
		t.Fatal(err)
	}
	prepared, published := false, false
	d.source.hooks = SwitchHooks{Prepare: func(context.Context) error { prepared = true; return lease.Close() }, Publish: func(ctx context.Context, l *Lease) error {
		var value string
		if err := l.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='proof'").Scan(&value); err != nil {
			return err
		}
		if value != "kept" || l.Generation() != 2 {
			t.Fatalf("candidate value=%q generation=%d", value, l.Generation())
		}
		if d.source.Ready() {
			t.Fatal("public admission opened before publication")
		}
		published = true
		return nil
	}}
	p, err := position(ctx, d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.switchTo(ctx, "standby", p); err != nil {
		t.Fatal(err)
	}
	if !prepared || !published || !d.source.Ready() || d.source.Generation() != 2 || m.Active != "standby" {
		t.Fatalf("incomplete switch prepare=%v publish=%v ready=%v role=%s generation=%d", prepared, published, d.source.Ready(), m.Active, d.source.Generation())
	}
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('new-authority','new')")
}

func TestFailoverPublicationFailureKeepsAdmissionClosed(t *testing.T) {
	d, _ := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('proof','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	d.source.hooks.Publish = func(context.Context, *Lease) error { return io.ErrUnexpectedEOF }
	p, err := position(ctx, d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.switchTo(ctx, "standby", p); err == nil {
		t.Fatal("failed publication accepted")
	}
	if d.source.Ready() {
		t.Fatal("failed public settings load opened admission")
	}
	if lease, err := d.source.Acquire(ctx, Reader); !errors.Is(err, ErrUnavailable) {
		if lease != nil {
			lease.Close()
		}
		t.Fatal("candidate publicly admitted", err)
	}
}
