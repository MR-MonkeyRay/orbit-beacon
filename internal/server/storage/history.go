package storage

import (
	"context"
	"database/sql"
)

const HistorySchemaVersion = 1
const historySchema = `CREATE TABLE buckets (
 node_id TEXT NOT NULL, metric TEXT NOT NULL, step INTEGER NOT NULL,
 t INTEGER NOT NULL, total REAL NOT NULL, peak REAL NOT NULL, samples INTEGER NOT NULL CHECK(samples > 0),
 PRIMARY KEY(node_id, metric, step, t)
) WITHOUT ROWID;
CREATE INDEX buckets_retention ON buckets(step, t);`

// OpenHistorySQLite 仅供离线恢复的私有暂存库使用。
func OpenHistorySQLite(ctx context.Context, path string) (*sql.DB, error) {
	return OpenWithMigrations(ctx, path, []Migration{{Version: HistorySchemaVersion, SQL: historySchema}})
}

func OpenHistory(ctx context.Context, path string) (*Database, error) {
	writer, err := OpenHistorySQLite(ctx, path)
	if err != nil {
		return nil, err
	}
	reader, err := OpenReader(ctx, path)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	return newDatabase(HistoryDomain, writer, reader), nil
}
