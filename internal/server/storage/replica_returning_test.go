package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReplicaSQLiteReturningLimitCannotCommitIgnoredConsumptionError(t *testing.T) {
	for _, mode := range []string{"row", "rows"} {
		t.Run(mode, func(t *testing.T) {
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
			value := strings.Repeat("x", 256<<10)
			if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES('large',?)", value); err != nil {
				t.Fatal(err)
			}
			for range 126 {
				if _, err = tx.ExecContext(ctx, "UPDATE settings SET value=? WHERE `key`='large'", value); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "row" {
				var got string
				err = tx.QueryRowContext(ctx, "UPDATE settings SET value=? WHERE `key`='large' RETURNING value", value).Scan(&got)
				if !errors.Is(err, ErrTransactionTooLarge) {
					t.Fatalf("Row.Scan error=%v", err)
				}
			} else {
				rows, e := tx.QueryContext(ctx, "UPDATE settings SET value=? WHERE `key`='large' RETURNING value", value)
				if e != nil && !errors.Is(e, ErrTransactionTooLarge) {
					t.Fatal(e)
				}
				// 调用者即使丢弃 Next/Err 的失败，也不能提交 FAIL 留下的语句修改。
				if rows != nil {
					for rows.Next() {
					}
					_ = rows.Close()
				}
			}
			if err = tx.Commit(); !errors.Is(err, ErrTransactionTooLarge) {
				t.Fatal("RETURNING committed oversized prefix", err)
			}
			var count, head int
			if err = source.current.reader.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM settings),head_seq FROM replica_state").Scan(&count, &head); err != nil || count != 0 || head != 0 {
				t.Fatalf("partial rows=%d head=%d err=%v", count, head, err)
			}
		})
	}
}
