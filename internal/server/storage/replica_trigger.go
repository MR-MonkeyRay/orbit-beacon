package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

func ensureCaptureTrigger(ctx context.Context, db *Database, name, query string) error {
	var existing string
	q := "SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?"
	if db.current.dialect == MySQL {
		q = "SELECT action_statement FROM information_schema.triggers WHERE trigger_schema=DATABASE() AND trigger_name=?"
	}
	err := db.current.reader.QueryRowContext(ctx, q, name).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return safeFailure(err)
	}
	canonical := func(s string) string {
		i := strings.Index(strings.ToUpper(s), "BEGIN ")
		if i >= 0 {
			s = s[i:]
		}
		return strings.Join(strings.Fields(strings.ToLower(s)), " ")
	}
	if err == nil && canonical(existing) == canonical(query) {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = db.current.writer.ExecContext(ctx, query)
		return safeFailure(err)
	}
	// 启动尚未开放业务；先跨 guard 证明旧事务结束，再替换本功能自己的固定触发器。
	barrier, err := db.current.writer.BeginTx(ctx, nil)
	if err != nil {
		return safeFailure(err)
	}
	defer barrier.Rollback()
	lock := "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1"
	if db.current.dialect == MySQL {
		lock += " FOR UPDATE"
	}
	var id string
	if err = barrier.QueryRowContext(ctx, lock).Scan(&id); err != nil {
		return safeFailure(err)
	}
	if db.current.dialect == SQLite {
		if _, err = barrier.ExecContext(ctx, "DROP TRIGGER `"+name+"`"); err != nil {
			return safeFailure(err)
		}
		if _, err = barrier.ExecContext(ctx, query); err != nil {
			return safeFailure(err)
		}
		return safeFailure(barrier.Commit())
	}
	if err = barrier.Rollback(); err != nil {
		return safeFailure(err)
	}
	if _, err = db.current.writer.ExecContext(ctx, "DROP TRIGGER `"+name+"`"); err != nil {
		return safeFailure(err)
	}
	_, err = db.current.writer.ExecContext(ctx, query)
	return safeFailure(err)
}

func replicaPacketLimit(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, d Dialect) (int64, error) {
	limit := int64(1 << 20)
	if d != MySQL {
		return limit, nil
	}
	var packet int64
	if err := query.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&packet); err != nil {
		return 0, safeFailure(err)
	}
	if packet/2 < limit {
		limit = packet / 2
	}
	if limit < 1 {
		return 0, ErrDeployment
	}
	return limit, nil
}
