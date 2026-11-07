package storage

import (
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLite 的 FAIL 保留当前事务内的越界计数；只按数值码和自己的事务头分类。
func (tx *Tx) captureFailure(err error) error {
	if err == nil {
		return nil
	}
	if tx.capture != nil {
		if code, ok := SQLiteCode(err); ok && code == sqlite3.SQLITE_CONSTRAINT_TRIGGER {
			var bytes int64
			if tx.tx.QueryRowContext(tx.ctx, "SELECT payload_bytes FROM replica_transactions WHERE tx_uuid=?", tx.capture.ID).Scan(&bytes) == nil && bytes > maxTransactionBytes {
				err = ErrTransactionTooLarge
			}
		}
		err = tx.lease.failure(err)
		tx.captureMu.Lock()
		if tx.captureError == nil {
			tx.captureError = err
		}
		tx.captureMu.Unlock()
	}
	return tx.lease.failure(err)
}
func (tx *Tx) capturedError() error {
	tx.captureMu.Lock()
	defer tx.captureMu.Unlock()
	return tx.captureError
}
func (tx *Tx) rejectPoisoned() error {
	if tx.capture != nil {
		return tx.capturedError()
	}
	return nil
}
