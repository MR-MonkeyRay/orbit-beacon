package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func freezePastBarrierWithoutSourceAccess(t *testing.T, d *replicaDomain, m *memberRecord) {
	t.Helper()
	ctx := context.Background()
	var boot string
	var barrier int64
	if err := d.target.current.reader.QueryRowContext(ctx, "SELECT boot_id,barrier_ns FROM replica_transactions t JOIN replica_state s ON t.epoch_uuid=s.epoch_uuid AND t.seq=s.head_seq WHERE s.id=1").Scan(&boot, &barrier); err != nil {
		t.Fatal(err)
	}
	if err := d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	frozen := freezePoint{Boot: boot, Nanos: barrier + int64(11*time.Second), Wall: time.Now().UTC()}
	if err := d.update(ctx, func(m *memberRecord) { m.Freeze = &frozen; m.FailureSince = frozen.Wall }); err != nil {
		t.Fatal(err)
	}
	d.control.cfg.Failover.MaxLossWindow = 0
	d.control.clock = func() (string, int64, time.Time, error) {
		return frozen.Boot, frozen.Nanos + int64(30*time.Second), frozen.Wall.Add(30 * time.Second), nil
	}
	// 源不能再提供 SQL guard；只有本进程已完整记录的可能提交边界可证明零缺口。
	if err := d.source.current.reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.source.current.writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFailoverCompletePossiblePrefixAllowsZeroLossWithoutSource(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('covered','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	freezePastBarrierWithoutSourceAccess(t, d, m)
	if err := d.tryPromotion(ctx); err != nil {
		t.Fatal("all possible committed SQL was already present", err)
	}
	if m.Active != "standby" || m.LossNanos == nil || *m.LossNanos != 0 {
		t.Fatal("complete possible prefix was not proved as exact zero loss", m.Active, m.LossNanos)
	}
	lease, err := d.source.Acquire(ctx, Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var value string
	if err = lease.QueryRowContext(lease.Context(), "SELECT value FROM settings WHERE `key`='covered'").Scan(&value); err != nil || value != "kept" {
		t.Fatal("zero-loss selected the wrong data", value, err)
	}
}

func TestFailoverUncoveredPossibleCommitCannotClaimZeroLoss(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('covered','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('uncovered','source-only')")
	freezePastBarrierWithoutSourceAccess(t, d, m)
	if err := d.tryPromotion(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("uncovered possible commit was not rejected", err)
	}
	if m.Active != "primary" || m.Intent != nil || d.source.Ready() {
		t.Fatal("zero proof discarded a real committed tail")
	}
}

func TestCancelledSwitchDoesNotPersistManualDrainTimeout(t *testing.T) {
	d, m := failoverPair(t)
	lease, err := d.source.Acquire(context.Background(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	selected, err := position(context.Background(), d.target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = d.switchTo(ctx, "standby", selected); !errors.Is(err, context.Canceled) {
		t.Fatal("switch did not respect shutdown cancellation", err)
	}
	if m.Manual || m.Intent != nil || m.Active != "primary" {
		t.Fatal("normal coordinator stop invented a manual drain timeout")
	}
}
