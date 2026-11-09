package storage

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

func captureTriggerBody(s string) string {
	if i := strings.Index(strings.ToUpper(s), "BEGIN "); i >= 0 {
		s = s[i:]
	}
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func captureTriggerBodies(ctx context.Context, db *Database) (map[string]string, error) {
	phase, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	query := "SELECT name,sql FROM sqlite_schema WHERE type='trigger'"
	if db.current.dialect == MySQL {
		query = "SELECT trigger_name,action_statement FROM information_schema.triggers WHERE trigger_schema=DATABASE()"
	}
	rows, err := db.current.reader.QueryContext(phase, query)
	if err != nil {
		return nil, safeFailure(err)
	}
	defer rows.Close()
	bodies := make(map[string]string)
	for rows.Next() {
		var name, body string
		if err = rows.Scan(&name, &body); err != nil {
			return nil, safeFailure(err)
		}
		bodies[name] = body
	}
	return bodies, safeFailure(rows.Err())
}

func ensureCaptureTrigger(ctx context.Context, db *Database, name, query, existing string, found bool) error {
	if found && captureTriggerBody(existing) == captureTriggerBody(query) {
		return nil
	}
	if !found {
		_, err := db.current.writer.ExecContext(ctx, query)
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
