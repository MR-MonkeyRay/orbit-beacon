package storage

import (
	"context"
	"database/sql/driver"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/config"
)

// 仅观察真实 FOR UPDATE；不替换 SQL 结果、时钟、事务或路由。
func ReplicaTestGuardBarrierSampling(t *testing.T, db *Database, endpoint config.DatabaseEndpoint) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var armed atomic.Bool
	var once sync.Once
	reached := make(chan struct{})
	if err := observeReplicaDatabase(ctx, db, &endpoint, func(_ context.Context, q string, _ []driver.NamedValue) {
		if armed.Load() && strings.HasPrefix(q, "SELECT last_tx_uuid FROM storage_write_guard") {
			once.Do(func() { close(reached) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.current.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var previous string
	if err = blocker.QueryRowContext(ctx, "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1 FOR UPDATE").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		lease, err := db.Acquire(ctx, Writer)
		if err != nil {
			result <- err
			return
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(ctx)
		if err != nil {
			result <- err
			return
		}
		defer tx.Rollback()
		result <- tx.Commit()
	}()
	defer func() { cancel(); blocker.Rollback(); <-finished }()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("writer did not reach its actual native guard", ctx.Err())
	}
	time.Sleep(150 * time.Millisecond)
	select {
	case err = <-result:
		t.Fatal("writer crossed an unreleased native guard", err)
	default:
	}
	if !db.Ready() {
		t.Fatal("ordinary guard contention froze a healthy domain")
	}
	boot, releasedAt, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, completedAt, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	var actualBoot string
	var barrier int64
	if err = db.current.reader.QueryRowContext(ctx, "SELECT boot_id,barrier_ns FROM replica_transactions t JOIN replica_state s ON t.epoch_uuid=s.epoch_uuid AND t.seq=s.head_seq WHERE s.id=1").Scan(&actualBoot, &barrier); err != nil {
		t.Fatal(err)
	}
	if actualBoot != boot || barrier < releasedAt || barrier > completedAt {
		t.Fatalf("transaction B was not sampled after its native guard: B=%d released=%d completed=%d sameBoot=%v", barrier, releasedAt, completedAt, actualBoot == boot)
	}
}
