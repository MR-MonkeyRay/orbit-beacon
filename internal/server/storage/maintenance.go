package storage

import (
	"context"
	"errors"

	sqlite3 "modernc.org/sqlite/lib"
)

var ErrCompactionUnsupported = errors.New("storage_compaction_unsupported")

// DiskUsage reports allocated SQL pages, excluding SQLite WAL/SHM; MySQL estimates table data and indexes.
func (l *Lease) DiskUsage(ctx context.Context) (size int64, reusable *int64, err error) {
	if l.Dialect() == MySQL {
		err = l.QueryRowContext(ctx, "SELECT COALESCE(SUM(DATA_LENGTH+INDEX_LENGTH),0) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()").Scan(&size)
		return
	}
	var pages, pageSize, free int64
	if err = l.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return
	}
	if err = l.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return
	}
	if err = l.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return
	}
	size, free = pages*pageSize, free*pageSize
	return size, &free, nil
}

// Compact retains this generation and serializes with all application writes, without a SQL transaction.
func (l *Lease) Compact(ctx context.Context) error {
	if l.Dialect() != SQLite {
		return ErrCompactionUnsupported
	}
	if l.pool != Writer {
		return ErrUnavailable
	}
	if pending, allowed := l.db.restoreAccess(ctx); pending && !allowed {
		return ErrRestorePending
	}
	release, err := l.retain()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := l.merge(ctx)
	defer cancel()
	select {
	case l.db.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.db.writeGate }()
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = l.db.resolvePending(ctx); err != nil {
		return err
	}
	conn, err := l.gen.writer.Conn(ctx)
	if err != nil {
		return l.failure(err)
	}
	defer conn.Close()
	checkpoint := func() error {
		var busy, log, done int
		if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done); err != nil {
			return l.failure(err)
		}
		if busy != 0 || log != done {
			return ErrRecoveryBusy
		}
		return nil
	}
	if err = checkpoint(); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "VACUUM"); err != nil {
		return l.vacuumFailure(err)
	}
	return checkpoint()
}

func (l *Lease) vacuumFailure(err error) error {
	// VACUUM's temporary rebuild can exhaust space without damaging the active database.
	if code, ok := SQLiteCode(err); ok && code == sqlite3.SQLITE_FULL {
		return safeFailure(err)
	}
	return l.failure(err)
}
