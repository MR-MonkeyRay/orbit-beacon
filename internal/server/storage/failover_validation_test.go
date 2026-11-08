package storage

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestFailoverValidationRechecksBarrierAfterMetadataStages(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('proof','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	boot, stamp, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	d.source = unavailableDatabase(MainDomain)
	d.source.replica = d
	m.Freeze = &freezePoint{Boot: boot, Nanos: stamp, Wall: time.Now().UTC()}
	var changed atomic.Bool
	if err = observeReplicaDatabase(ctx, d.target, nil, func(ctx context.Context, q string, _ []driver.NamedValue) {
		if strings.Contains(q, "FROM sqlite_schema WHERE type IN") && changed.CompareAndSwap(false, true) {
			// 在完整定义校验期间实际改变候选头的 boot；早先采样的 B 不能继续作证明。
			if _, err := d.target.current.writer.ExecContext(ctx, "UPDATE replica_transactions SET boot_id=?", uuid.NewString()); err != nil {
				t.Error(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.promotionCheckpoint(ctx, *m); err == nil {
		t.Fatal("promotion reused the pre-validation barrier")
	}
	if !changed.Load() {
		t.Fatal("real metadata validation did not run")
	}
}

func TestFailoverCandidateRejectsMissingCaptureTriggerWithoutRepair(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('proof','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	boot, stamp, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	m.Freeze = &freezePoint{Boot: boot, Nanos: stamp, Wall: time.Now().UTC()}
	if _, err = d.target.current.writer.ExecContext(ctx, "DROP TRIGGER ob_replica_1_i"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.promotionCheckpoint(ctx, *m); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatal("incomplete capture schema was accepted", err)
	}
	var count int
	if err = d.target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name='ob_replica_1_i'").Scan(&count); err != nil || count != 0 {
		t.Fatal("promotion silently repaired unverified definitions", count, err)
	}
}

func TestFailoverInterruptedRebuildPersistsIncompleteOperation(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('rebuild','source')")
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var interrupted atomic.Bool
	if err := observeReplicaDatabase(ctx, d.target, nil, func(_ context.Context, q string, _ []driver.NamedValue) {
		if strings.HasPrefix(q, "INSERT INTO `settings`") && interrupted.CompareAndSwap(false, true) {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.seed(copyCtx); err == nil {
		t.Fatal("interrupted real copy succeeded")
	}
	if !interrupted.Load() {
		t.Fatal("copy never reached its real application insert")
	}
	if m.Ready || m.Rebuild == nil || m.Rebuild.Phase != "copying" {
		t.Fatal("partial target lost durable rebuild identity", m.Ready, m.Rebuild)
	}
	r, err := openControl(ctx, d.control.dataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.control.Close()
	recovered := r.record.Domains[MainDomain]
	if recovered.Ready || recovered.Rebuild == nil || recovered.Rebuild.ID != m.Rebuild.ID {
		t.Fatal("restart regarded an incomplete snapshot as ready")
	}
	// 从同一 active 重新开始，不能采用上次中断的半份数据。
	if err = d.seed(ctx); err != nil {
		t.Fatal(err)
	}
	var value string
	if err = d.target.current.reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='rebuild'").Scan(&value); err != nil || value != "source" {
		t.Fatal("restarted copy did not converge", value, err)
	}
	if !m.Ready || m.Rebuild != nil {
		t.Fatal("finished rebuild remained incomplete")
	}
}
