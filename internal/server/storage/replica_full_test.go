package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

func TestReplicaSQLiteFullJournalRollsBackBusinessAndCheckpoint(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := context.Background()
	value := strings.Repeat("q", 128<<10)
	l, err := source.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := l.BeginWrite(ctx)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES('quota',?)", value); err != nil {
		tx.Rollback()
		l.Close()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		l.Close()
		t.Fatal(err)
	}
	l.Close()
	target.current.writer.SetMaxOpenConns(1)
	target.current.writer.SetMaxIdleConns(1)
	var pages, pageSize, maximum int
	if err = target.current.writer.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err = target.current.writer.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	limit := pages + len(value)/pageSize + 16
	if err = target.current.writer.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", limit)).Scan(&maximum); err != nil || maximum != limit {
		t.Fatalf("quota fixture max=%d err=%v", maximum, err)
	}
	// 原行有空间，追加完整 after-image 日志没有空间；使用引擎真实 SQLITE_FULL。
	batch, err := readReplicaTransaction(ctx, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = applyReplicaTransaction(ctx, target, batch)
	if code, ok := SQLiteCode(err); !ok || code != sqlite3.SQLITE_FULL {
		t.Fatalf("full journal error=%v code=%d", err, code)
	}
	var rows, logs, head int
	if err = target.current.reader.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM settings),(SELECT count(*) FROM replica_transactions),head_seq FROM replica_state").Scan(&rows, &logs, &head); err != nil || rows != 0 || logs != 0 || head != 0 {
		t.Fatalf("failed target advanced rows=%d logs=%d head=%d err=%v", rows, logs, head, err)
	}
}
