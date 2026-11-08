package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func mirrorPressureFixture(t *testing.T, source, target *Database) {
	t.Helper()
	ctx := context.Background()
	var index int
	var name, path string
	if err := source.current.reader.QueryRowContext(ctx, "PRAGMA database_list").Scan(&index, &name, &path); err != nil {
		t.Fatal(err)
	}
	conn, err := target.current.writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "ATTACH DATABASE ? AS pressure_snapshot", path); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "DETACH DATABASE pressure_snapshot")
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		"INSERT INTO main.settings SELECT * FROM pressure_snapshot.settings",
		"DELETE FROM main.replica_changes",
		"DELETE FROM main.replica_transactions",
		"DELETE FROM main.replica_epochs",
		"INSERT INTO main.replica_epochs SELECT * FROM pressure_snapshot.replica_epochs",
		"INSERT INTO main.replica_transactions SELECT * FROM pressure_snapshot.replica_transactions",
		"INSERT INTO main.replica_changes SELECT * FROM pressure_snapshot.replica_changes",
		"DELETE FROM main.replica_state",
		"INSERT INTO main.replica_state SELECT * FROM pressure_snapshot.replica_state",
		"UPDATE main.replica_state SET capture_mode='off',ready=1 WHERE id=1",
		"UPDATE main.storage_write_guard SET last_tx_uuid=(SELECT last_tx_uuid FROM pressure_snapshot.storage_write_guard WHERE id=1) WHERE id=1",
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestReceiverBoundedAncestorGCResumesExactLegalBatchWithoutManualLock(t *testing.T) {
	source := ancestorPressureFixture(t)
	target := source.replica.target
	ctx := context.Background()
	mirrorPressureFixture(t, source, target)
	// 两端同一完整业务快照；源先回收自己的旧日志，接收端仍保留真实的240MiB祖先日志。
	empty := false
	for range 256 {
		if err := source.replica.prune(ctx, time.Now().Add(25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var bytes int64
		if err := source.current.reader.QueryRowContext(ctx, "SELECT journal_bytes FROM replica_state WHERE id=1").Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		if bytes == 0 {
			empty = true
			break
		}
	}
	if !empty {
		t.Fatal("source did not finish its actual bounded ancestor reclamation")
	}
	large := strings.Repeat("r", 64<<10)
	lease, err := source.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES('receiver-large',?)", large); err != nil {
		tx.Rollback()
		lease.Close()
		t.Fatal(err)
	}
	for range 495 {
		if _, err = tx.ExecContext(ctx, "UPDATE settings SET value=value WHERE `key`='receiver-large'"); err != nil {
			tx.Rollback()
			lease.Close()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		lease.Close()
		t.Fatal(err)
	}
	lease.Close()
	batch, err := readReplicaTransaction(ctx, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Bytes > maxTransactionBytes || batch.Bytes < 60<<20 {
		t.Fatal("source did not capture a real legal large transaction", batch.Bytes)
	}
	if batch.Count != 496 || len(batch.Changes) != 496 {
		t.Fatal("source did not capture every change in the single large transaction", batch.Count, len(batch.Changes))
	}
	for _, change := range batch.Changes {
		if len(change.PK)+len(change.After) > 256<<10 {
			t.Fatal("fixture change exceeded its bounded statement transport", len(change.PK)+len(change.After))
		}
	}
	r := replicaReviewControl(t, source, target, true, "healthy")
	run, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() { source.replica.run(run); close(stopped) }()
	defer func() { cancel(); <-stopped }()
	applied := false
	for run.Err() == nil {
		r.mu.Lock()
		manual := r.record.Domains[MainDomain].Manual
		r.mu.Unlock()
		if manual {
			t.Fatal("legal receiver batch was permanently misclassified as oversized/integrity failure")
		}
		var count int
		if err = target.current.reader.QueryRowContext(run, "SELECT count(*) FROM settings WHERE `key`='receiver-large'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			applied = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !applied {
		t.Fatal("receiver never committed its original exact-position batch", run.Err())
	}
	cancel()
	<-stopped
	before, err := position(ctx, target.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if before.Epoch != batch.Epoch || before.Seq < batch.Seq {
		t.Fatal("receiver data committed without its source head", before, batch.replicaPosition)
	}
	if err = applyReplicaTransaction(ctx, target, batch); err != nil {
		t.Fatal("duplicate source UUID was not idempotent", err)
	}
	after, err := position(ctx, target.current.reader)
	if err != nil || after != before {
		t.Fatal("duplicate application advanced the receiver", after, before, err)
	}
	var bytes, sum int64
	var length, logs int
	if err = target.current.reader.QueryRowContext(ctx, "SELECT journal_bytes,(SELECT COALESCE(SUM(payload_bytes),0) FROM replica_transactions) FROM replica_state WHERE id=1").Scan(&bytes, &sum); err != nil || bytes != sum || bytes > maxJournalBytes {
		t.Fatal("receiver maintenance lost its accounting", bytes, sum, err)
	}
	if err = target.current.reader.QueryRowContext(ctx, "SELECT length(value) FROM settings WHERE `key`='receiver-large'").Scan(&length); err != nil || length != len(large) {
		t.Fatal("receiver committed partial data", length, err)
	}
	if err = target.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE tx_uuid=?", batch.ID).Scan(&logs); err != nil || logs != 1 {
		t.Fatal("receiver replayed the source transaction", logs, err)
	}
	if !source.Ready() {
		t.Fatal("receiver housekeeping froze the healthy source")
	}
}
