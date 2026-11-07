package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

const sqliteGuardSchema = `CREATE TABLE storage_write_guard(id INTEGER PRIMARY KEY CHECK(id=1),last_tx_uuid TEXT NOT NULL,restore_operation TEXT NOT NULL); INSERT INTO storage_write_guard VALUES(1,'','');`
const mysqlGuardSchema = `CREATE TABLE storage_write_guard(id BIGINT PRIMARY KEY CHECK(id=1),last_tx_uuid VARBINARY(64) NOT NULL,restore_operation VARBINARY(64) NOT NULL) ENGINE=InnoDB`

var ErrCommitUnknown = errors.New("storage_commit_unknown")

type commitReceipt struct {
	mu               sync.Mutex
	id               string
	known, committed bool
}

func (tx *Tx) Dialect() Dialect { return tx.lease.Dialect() }
func (tx *Tx) ID() string {
	if tx.receipt == nil {
		return ""
	}
	return tx.receipt.id
}

// ResolveCommit 可在原租约释放后调用；只有新 guard 屏障能证明旧 COMMIT 已结束。
func (tx *Tx) ResolveCommit(ctx context.Context) (bool, error) {
	if tx.receipt == nil {
		return false, sql.ErrTxDone
	}
	tx.receipt.mu.Lock()
	known, committed := tx.receipt.known, tx.receipt.committed
	tx.receipt.mu.Unlock()
	if known {
		return committed, nil
	}
	db := tx.lease.db
	select {
	case db.writeGate <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-db.writeGate }()
	if err := db.resolvePending(ctx); err != nil {
		return false, err
	}
	tx.receipt.mu.Lock()
	defer tx.receipt.mu.Unlock()
	if !tx.receipt.known {
		return false, ErrCommitUnknown
	}
	return tx.receipt.committed, nil
}

// 调用者持有域 writeGate，解析完成前收据不会被后续事务覆盖。
func (db *Database) resolvePending(ctx context.Context) error {
	db.mu.Lock()
	pending := db.pending
	gen := db.current
	closed := db.closed
	db.mu.Unlock()
	if pending == nil {
		return nil
	}
	if closed {
		return ErrUnavailable
	}
	conn, err := gen.writer.Conn(ctx)
	if err != nil {
		return ErrCommitUnknown
	}
	defer conn.Close()
	barrier, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ErrCommitUnknown
	}
	defer barrier.Rollback()
	var id string
	if err = barrier.QueryRowContext(ctx, "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1 FOR UPDATE").Scan(&id); err != nil {
		return ErrCommitUnknown
	}
	// 只读屏障无需提交：持锁查询已证明前一事务完成。
	if err = barrier.Rollback(); err != nil {
		return ErrCommitUnknown
	}
	pending.mu.Lock()
	pending.known = true
	pending.committed = id == pending.id
	pending.mu.Unlock()
	db.mu.Lock()
	if db.pending == pending {
		db.pending = nil
	}
	db.mu.Unlock()
	return nil
}

func (db *Database) InstanceID(ctx context.Context) (string, error) {
	l, err := db.Acquire(ctx, Reader)
	if err != nil {
		return "", err
	}
	defer l.Close()
	if l.Dialect() != MySQL {
		return "", nil
	}
	var id string
	err = l.QueryRowContext(ctx, "SELECT @@server_uuid").Scan(&id)
	return id, safeFailure(err)
}
func (db *Database) IsEmpty(ctx context.Context) (bool, error) {
	l, err := db.Acquire(ctx, Reader)
	if err != nil {
		return false, err
	}
	defer l.Close()
	tx, err := l.BeginRead(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, table := range Tables(db.domain) {
		if table.Name == "inbox_sequence" {
			continue
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM `"+table.Name+"`").Scan(&count); err != nil {
			return false, safeFailure(err)
		}
		if count != 0 {
			return false, nil
		}
	}
	return true, nil
}
