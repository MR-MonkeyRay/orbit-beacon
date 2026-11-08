package storage

import (
	"context"
	"testing"
	"time"
)

func TestFailoverAncestorGCRollbackKeepsDeletionRetryable(t *testing.T) {
	d, _ := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('ancestor','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	parent, err := position(ctx, d.source.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	if err = d.switchTo(ctx, "standby", parent); err != nil {
		t.Fatal(err)
	}
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('current','kept')")
	if err = d.synchronize(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := d.source.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES('rolled-back','absent')"); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(25 * time.Hour)
	if err = d.pruneTx(ctx, tx.tx, future, 0); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=?", parent.Epoch).Scan(&count); err != nil || count == 0 {
		t.Fatal("rollback did not restore the ancestor log", count, err)
	}
	if err = d.prune(ctx, future); err != nil {
		t.Fatal(err)
	}
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=?", parent.Epoch).Scan(&count); err != nil || count != 0 {
		t.Fatal("rolled-back cleanup was permanently skipped by its cache", count, err)
	}
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE `key`='rolled-back'").Scan(&count); err != nil || count != 0 {
		t.Fatal("cleanup committed a rolled-back business write", count, err)
	}
}
