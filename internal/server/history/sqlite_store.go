package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"orbitbeacon/internal/server/storage"
)

const cleanupBatchSize = 1000

// 定位到下一个主键节点前缀，而不是为 DISTINCT 遍历每个桶。
// 工作量随节点数增长，与保留的历史无关。
const historyNodesQuery = `WITH RECURSIVE history_nodes(node_id) AS (
 SELECT min(node_id) FROM buckets
 UNION ALL
 SELECT (SELECT min(node_id) FROM buckets WHERE node_id > history_nodes.node_id)
 FROM history_nodes WHERE node_id IS NOT NULL
) SELECT node_id FROM history_nodes WHERE node_id IS NOT NULL`

const schema = `CREATE TABLE buckets (
 node_id TEXT NOT NULL, metric TEXT NOT NULL, step INTEGER NOT NULL,
 t INTEGER NOT NULL, total REAL NOT NULL, peak REAL NOT NULL, samples INTEGER NOT NULL CHECK(samples > 0),
 PRIMARY KEY(node_id, metric, step, t)
) WITHOUT ROWID;
CREATE INDEX buckets_retention ON buckets(step, t);`

// historySeriesQuery 沿主键跳跃，只按序列数访问 B 树；DISTINCT 会扫完该节点的全部桶，
// 重启后每个节点都要读一次，且运行在历史写入 goroutine 上。
const historySeriesQuery = `WITH RECURSIVE series(metric) AS (
	SELECT min(metric) FROM buckets WHERE node_id=?1
	UNION ALL
	SELECT (SELECT min(metric) FROM buckets WHERE node_id=?1 AND metric>series.metric) FROM series WHERE series.metric IS NOT NULL
) SELECT metric FROM series WHERE metric IS NOT NULL`

type sqliteStore struct {
	db, reader *sql.DB
}

var _ Store = (*sqliteStore)(nil)

// OpenDatabase 只初始化可信的指标 schema，不启动写入或定期清理任务。
// 离线恢复用它创建私有暂存库。
func OpenDatabase(ctx context.Context, path string) (*sql.DB, error) {
	return storage.OpenWithMigrations(ctx, path, []storage.Migration{{Version: CurrentSchemaVersion, SQL: schema}})
}

func openSQLiteStore(ctx context.Context, path string) (*sqliteStore, error) {
	db, err := OpenDatabase(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open metric history: %w", err)
	}
	reader, err := storage.OpenReader(ctx, path)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open metric history reader: %w", err)
	}
	return &sqliteStore{db: db, reader: reader}, nil
}

// Write 在同一事务内合并三个层级；失败时仍报告计划写入的行数。
func (s *sqliteStore) Write(ctx context.Context, buckets []Bucket) (rows int, err error) {
	rows = 3 * len(buckets)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rows, err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES(?,?,?,?,?,?,?) ON CONFLICT(node_id,metric,step,t) DO UPDATE SET total=total+excluded.total, peak=max(peak,excluded.peak), samples=samples+excluded.samples`)
	if err != nil {
		return rows, err
	}
	defer statement.Close()
	for _, bucket := range buckets {
		for _, step := range []int64{60, 300, 3600} {
			t := time.Unix(bucket.T, 0).Truncate(time.Duration(step) * time.Second).Unix()
			if _, err := statement.ExecContext(ctx, bucket.Node, bucket.Series, step, t, bucket.Total, bucket.Peak, bucket.Samples); err != nil {
				return rows, err
			}
		}
	}
	return rows, tx.Commit()
}

func (s *sqliteStore) Query(ctx context.Context, node, series string, stepSeconds int, start, end int64) ([]Point, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT t, total / samples, peak FROM buckets WHERE node_id=? AND metric=? AND step=? AND t>=? AND t<=? ORDER BY t`, node, series, stepSeconds, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []Point{}
	for rows.Next() {
		var point Point
		var timestamp int64
		if err := rows.Scan(&timestamp, &point.Avg, &point.Max); err != nil {
			return nil, err
		}
		point.T = time.Unix(timestamp, 0).UTC()
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return points, nil
}

func (s *sqliteStore) Series(ctx context.Context, node string) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, historySeriesQuery, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var series []string
	for rows.Next() {
		var metric string
		if err := rows.Scan(&metric); err != nil {
			return nil, err
		}
		series = append(series, metric)
	}
	return series, rows.Err()
}

func (s *sqliteStore) NodeIDs(ctx context.Context, visit func(string)) error {
	rows, err := s.db.QueryContext(ctx, historyNodesQuery)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		visit(id)
	}
	return rows.Err()
}

// 每个 Exec 都是自己的短自动提交事务。该表是 WITHOUT ROWID，
// 因此每次删除最多选择一批复合主键。
func (s *sqliteStore) deleteBatches(ctx context.Context, yield func(), query string, args ...any) (removed int64, err error) {
	for {
		result, err := s.db.ExecContext(ctx, query, args...)
		if err != nil {
			return removed, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return removed, err
		}
		removed += n
		yield()
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if n < cleanupBatchSize {
			return removed, nil
		}
	}
}

func (s *sqliteStore) DeleteNode(ctx context.Context, node string, yield func()) (int64, error) {
	return s.deleteBatches(ctx, yield, `DELETE FROM buckets WHERE (node_id,metric,step,t) IN (SELECT node_id,metric,step,t FROM buckets WHERE node_id=? LIMIT ?)`, node, cleanupBatchSize)
}

func (s *sqliteStore) DeleteExpired(ctx context.Context, now time.Time, yield func()) (int64, error) {
	var removed int64
	for _, tier := range []struct {
		step int
		keep time.Duration
	}{{60, Retention1m}, {300, Retention5m}, {3600, Retention1h}} {
		n, err := s.deleteBatches(ctx, yield, `DELETE FROM buckets WHERE (node_id,metric,step,t) IN (SELECT node_id,metric,step,t FROM buckets WHERE step=? AND t<? LIMIT ?)`, tier.step, now.Add(-tier.keep).Truncate(time.Duration(tier.step)*time.Second).Unix(), cleanupBatchSize)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (s *sqliteStore) DatabaseStats() (writer, reader sql.DBStats, ok bool) {
	return s.db.Stats(), s.reader.Stats(), true
}

func (s *sqliteStore) Close() error {
	return errors.Join(s.reader.Close(), s.db.Close())
}
