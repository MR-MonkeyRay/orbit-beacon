package storage

import (
	"context"
	"database/sql/driver"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAncestorGCUsesBoundedCanonicalCursorAcrossLongEmptyChain(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := context.Background()
	replicaWrite(t, source, "INSERT INTO settings VALUES('long-lineage','kept')")
	if err := source.replica.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := position(ctx, target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	// 协议形状夹具：真实已复制事务之后是无业务变化的合法空 epoch，不改角色或 generation。
	tx, err := target.current.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	parent := original
	for range 128 {
		next := uuid.NewString()
		if _, err = tx.ExecContext(ctx, "INSERT INTO replica_epochs VALUES(?,?,?,?)", next, parent.Epoch, parent.Seq, parent.ID); err != nil {
			t.Fatal(err)
		}
		parent = replicaPosition{Epoch: next}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid=?,head_seq=0,head_tx_uuid='',retained_from_seq=1,capture_mode='off',ready=1 WHERE id=1", parent.Epoch); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	var scanned atomic.Bool
	if err = observeReplicaDatabase(ctx, target, nil, func(call context.Context, q string, values []driver.NamedValue) {
		upper := strings.ToUpper(q)
		if strings.HasPrefix(upper, "EXPLAIN") || (!strings.Contains(upper, "REPLICA_EPOCHS") && !strings.Contains(upper, "REPLICA_TRANSACTIONS")) {
			return
		}
		queries.Add(1)
		if !strings.HasPrefix(upper, "SELECT") || !strings.Contains(upper, "REPLICA_EPOCHS") {
			return
		}
		args := make([]any, len(values))
		for i, v := range values {
			args[i] = v.Value
		}
		rows, err := target.current.reader.QueryContext(call, "EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Error(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(strings.ToUpper(detail), "SCAN") {
				scanned.Store(true)
			}
		}
		if err = rows.Err(); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	pruner := &replicaDomain{source: target}
	maxQueries := int64(0)
	goneAt := -1
	for call := range 64 {
		queries.Store(0)
		if err = pruner.prune(ctx, time.Now().Add(25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if count := queries.Load(); count > maxQueries {
			maxQueries = count
		}
		var count int
		if err = target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=?", original.Epoch).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 && goneAt < 0 {
			goneAt = call
		}
		if goneAt >= 0 && call >= goneAt+2 {
			break
		}
	}
	if scanned.Load() {
		t.Error("ancestor GC scanned the complete epoch directory inside the writer gate")
	}
	if maxQueries > 32 {
		t.Errorf("one GC call walked an unbounded empty lineage: %d protocol statements", maxQueries)
	}
	if goneAt < 0 {
		t.Fatal("bounded calls never reached the expired canonical ancestor")
	}
	actual, err := position(ctx, target.current.reader)
	if err != nil || actual != parent {
		t.Fatal("GC changed the selected head", actual, err)
	}
	var bytes, sum int64
	var rows int
	if err = target.current.reader.QueryRowContext(ctx, "SELECT journal_bytes,(SELECT COALESCE(SUM(payload_bytes),0) FROM replica_transactions) FROM replica_state WHERE id=1").Scan(&bytes, &sum); err != nil || bytes != sum || bytes != 0 {
		t.Fatal("bounded cleanup lost its accounting", bytes, sum, err)
	}
	if err = target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE `key`='long-lineage' AND value='kept'").Scan(&rows); err != nil || rows != 1 {
		t.Fatal("GC changed business data", rows, err)
	}
}
