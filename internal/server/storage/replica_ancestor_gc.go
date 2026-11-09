package storage

import (
	"context"
	"database/sql"
	"errors"
)

// 游标仅记不可变父链的遍历位置；每次最多查看8个祖先，不推断未提交删除已生效。
func (d *replicaDomain) pruneAncestors(ctx context.Context, tx *sql.Tx, epoch, boot string, current, age, total, incoming int64) (int64, error) {
	db := d.source
	if db.journalEpoch != epoch {
		db.journalEpoch, db.journalCursor = epoch, epoch
	}
	if db.journalCursor == "" {
		return 0, nil
	}
	var removed int64
	for step := range 8 {
		cursor := db.journalCursor
		var ancestor string
		if err := tx.QueryRowContext(ctx, "SELECT parent_epoch_uuid FROM replica_epochs WHERE epoch_uuid=?", cursor).Scan(&ancestor); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return removed, ErrReplicaDiverged
			}
			return removed, safeFailure(err)
		}
		if ancestor == "" {
			if cursor == epoch {
				db.journalCursor = ""
				return removed, nil
			}
			db.journalCursor = epoch
			if step == 0 {
				continue
			}
			return removed, nil
		}
		if ancestor == epoch || ancestor == cursor {
			return removed, ErrReplicaDiverged
		}
		rows, err := tx.QueryContext(ctx, "SELECT seq,payload_bytes,boot_id,barrier_ns FROM replica_transactions WHERE epoch_uuid=? AND seq>0 ORDER BY seq LIMIT 256", ancestor)
		if err != nil {
			return removed, safeFailure(err)
		}
		var last, bytes int64
		for rows.Next() {
			var seq, size, barrier int64
			var b string
			if err = rows.Scan(&seq, &size, &b, &barrier); err != nil {
				rows.Close()
				return removed, safeFailure(err)
			}
			if b == boot && current-barrier <= age && total+incoming-removed-bytes <= maxJournalBytes {
				break
			}
			last = seq
			bytes += size
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return removed, safeFailure(err)
		}
		if last != 0 {
			// 当前epoch的pin/收据仍由原清理路径保护，祖先只保留不可变分叉元数据。
			if _, err = tx.ExecContext(ctx, "DELETE FROM replica_transactions WHERE epoch_uuid=? AND seq<=?", ancestor, last); err != nil {
				return removed, safeFailure(err)
			}
			removed += bytes
		}
		db.journalCursor = ancestor
	}
	return removed, nil
}

// 实际入站大小只是回收需求；业务回滚后仍保留，不代表提交位置或复制资格。
func (db *Database) journalPressure(incoming int64) error {
	if incoming <= 0 || incoming > maxTransactionBytes {
		return ErrTransactionTooLarge
	}
	db.journalRequired = max(db.journalRequired, incoming)
	return errReplicaJournalPressure
}

func (db *Database) journalCommitted(bytes int64) {
	if bytes > 0 && bytes >= db.journalRequired {
		db.journalRequired = 0
	}
}
