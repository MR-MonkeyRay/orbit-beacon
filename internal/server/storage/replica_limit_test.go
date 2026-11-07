package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReplicaSQLiteTriggerLimitIsTypedAndAtomic(t *testing.T) {
	source, _ := replicaPair(t, MainDomain)
	ctx := context.Background()
	l, err := source.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tx, err := l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO settings VALUES(?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	value := strings.Repeat("x", 256<<10)
	for i := range 140 {
		_, err = stmt.ExecContext(ctx, string(rune('a'+i)), value)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrTransactionTooLarge) {
		t.Fatalf("trigger error=%v", err)
	}
	if err = tx.Commit(); !errors.Is(err, ErrTransactionTooLarge) {
		t.Fatalf("poisoned transaction committed: %v", err)
	}
	var rows, logs, head int
	if err = source.current.reader.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM settings),(SELECT count(*) FROM replica_transactions),head_seq FROM replica_state").Scan(&rows, &logs, &head); err != nil || rows != 0 || logs != 0 || head != 0 {
		t.Fatalf("limit was not atomic rows=%d logs=%d head=%d err=%v", rows, logs, head, err)
	}
}
