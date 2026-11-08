package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ancestorPressureFixture(t *testing.T) *Database {
	t.Helper()
	source, _ := replicaPair(t, MainDomain)
	ctx := context.Background()
	value := strings.Repeat("a", 1536<<10)
	lease, err := source.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := lease.BeginWrite(ctx)
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if _, err = initial.ExecContext(ctx, "INSERT INTO settings VALUES('pressure',?)", value); err != nil {
		initial.Rollback()
		lease.Close()
		t.Fatal(err)
	}
	if err = initial.Commit(); err != nil {
		lease.Close()
		t.Fatal(err)
	}
	lease.Close()
	// 每个祖先都含真实捕获/提交的约3MiB事务；空fork marker只构造GC形状，不控制业务路由。
	for step := range 80 {
		if step > 0 {
			replicaWrite(t, source, "UPDATE settings SET value=value WHERE `key`='pressure'")
		}
		p, err := position(ctx, source.current.reader)
		if err != nil {
			t.Fatal(err)
		}
		next := uuid.NewString()
		if err = source.offlineGuard(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO replica_epochs VALUES(?,?,?,?)", next, p.Epoch, p.Seq, p.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid=?,head_seq=0,head_tx_uuid='',retained_from_seq=1 WHERE id=1", next)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		source.replica.epoch = next
	}
	var before int64
	if err = source.current.reader.QueryRowContext(ctx, "SELECT journal_bytes FROM replica_state WHERE id=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before < 230<<20 || before > maxJournalBytes {
		t.Fatal("fixture did not reach real journal pressure", before)
	}
	return source
}

func TestBoundedAncestorGCDoesNotPermanentlyRejectLegalLargeTransaction(t *testing.T) {
	source := ancestorPressureFixture(t)
	ctx := context.Background()
	var err error
	large := strings.Repeat("b", 31<<20)
	committed := false
	for range 16 {
		lease, err := source.Acquire(ctx, Writer)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := lease.BeginWrite(ctx)
		if err != nil {
			lease.Close()
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES('legal-large',?)", large); err == nil {
			err = tx.Commit()
		}
		tx.Rollback()
		lease.Close()
		if err == nil {
			committed = true
			break
		}
		if !errors.Is(err, ErrTransactionTooLarge) && !IsBusy(err) {
			t.Fatal(err)
		}
		if !source.Ready() {
			t.Fatal("bounded housekeeping retired a healthy source")
		}
		// 一次真实后台GC提交，然后显式重试已确定回滚的测试事务；不自动重跑生产业务。
		if err = source.replica.prune(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if !committed {
		t.Fatal("a legal <64MiB transaction remained permanently blocked by rolled-back bounded ancestor cleanup")
	}
	var bytes, sum int64
	var length int
	if err = source.current.reader.QueryRowContext(ctx, "SELECT journal_bytes,(SELECT COALESCE(SUM(payload_bytes),0) FROM replica_transactions) FROM replica_state WHERE id=1").Scan(&bytes, &sum); err != nil || bytes != sum || bytes > maxJournalBytes {
		t.Fatal("pressure cleanup corrupted its accounting", bytes, sum, err)
	}
	if err = source.current.reader.QueryRowContext(ctx, "SELECT length(value) FROM settings WHERE `key`='legal-large'").Scan(&length); err != nil || length != len(large) {
		t.Fatal("legal transaction was not fully committed", length, err)
	}
}
