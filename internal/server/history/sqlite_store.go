package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

// historySeriesQuery 沿主键跳跃，只按序列数访问 B 树；DISTINCT 会扫完该节点的全部桶，
// 重启后每个节点都要读一次，且运行在历史写入 goroutine 上。
const historySeriesQuery = `WITH RECURSIVE series(metric) AS (
	SELECT min(metric) FROM buckets WHERE node_id=?1
	UNION ALL
	SELECT (SELECT min(metric) FROM buckets WHERE node_id=?1 AND metric>series.metric) FROM series WHERE series.metric IS NOT NULL
) SELECT metric FROM series WHERE metric IS NOT NULL`

type sqliteStore struct {
	db         *storage.Database
	nodes      *storage.Database
	writeMu    sync.Mutex
	unresolved *storage.Tx
	inflight   []Bucket
}

var _ Store = (*sqliteStore)(nil)

// OpenDatabase 只初始化可信的指标 schema，不启动写入或定期清理任务。
// 离线恢复用它创建私有暂存库。
func OpenDatabase(ctx context.Context, path string) (*sql.DB, error) {
	return storage.OpenHistorySQLite(ctx, path)
}

func openSQLiteStore(ctx context.Context, path string) (*sqliteStore, error) {
	db, err := storage.OpenHistory(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open metric history: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

// Write 在同一事务内合并三个层级；失败时仍报告计划写入的行数。
func (s *sqliteStore) Write(ctx context.Context, buckets []Bucket) (rows int, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.unresolved != nil {
		if !slices.Equal(s.inflight, buckets) {
			return 3 * len(buckets), storage.ErrCommitUnknown
		}
		committed, resolveErr := s.resolveInflight(ctx)
		if resolveErr != nil {
			return 3 * len(buckets), resolveErr
		}
		if committed {
			return 3 * len(buckets), nil
		}
	}
	lease, err := s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return 3 * len(buckets), err
	}
	defer lease.Close()
	ctx = lease.Context()
	rows = 3 * len(buckets)
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return rows, err
	}
	defer tx.Rollback()
	if tx.Dialect() == storage.MySQL {
		for next := 0; next < rows; {
			query, args, end, err := mysqlHistoryBatch(buckets, next)
			if err != nil {
				return rows, err
			}
			if _, err = tx.ExecContext(ctx, query, args...); err != nil {
				return rows, err
			}
			next = end
		}
		return rows, s.commitHistory(tx, buckets)
	}
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
	return rows, s.commitHistory(tx, buckets)
}

// 未知提交仅保留原 UUID；Write 返回时释放语句、事务和租约，下次只核对原批次。
func (s *sqliteStore) commitHistory(tx *storage.Tx, buckets []Bucket) error {
	err := tx.Commit()
	if errors.Is(err, storage.ErrCommitUnknown) {
		s.unresolved, s.inflight = tx, slices.Clone(buckets)
	}
	return err
}

// 不持 lease 等待旧收据；确定回滚后由 Service 重新校验 TTL、墓碑及当前权威。
func (s *sqliteStore) resolveInflight(ctx context.Context) (bool, error) {
	committed, err := s.unresolved.ResolveCommit(ctx)
	if err != nil {
		return false, errors.Join(storage.ErrCommitUnknown, err)
	}
	s.unresolved, s.inflight = nil, nil
	return committed, nil
}

const mysqlHistoryInsert = "INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES "
const mysqlHistoryUpdate = " ON DUPLICATE KEY UPDATE total=buckets.total+VALUES(total),peak=GREATEST(buckets.peak,VALUES(peak)),samples=buckets.samples+VALUES(samples)"

// 估算驱动插值的最坏编码尺寸，包含二进制/文本转义及数值。
func historyArgsBytes(args []any) int {
	n := 0
	for _, arg := range args {
		if text, ok := arg.(string); ok {
			n += 2*len(text) + 2
		} else {
			n += 32
		}
	}
	return n
}

func mysqlHistoryBatch(buckets []Bucket, start int) (string, []any, int, error) {
	var values strings.Builder
	args := make([]any, 0, 256*7)
	bytes, end := len(mysqlHistoryInsert)+len(mysqlHistoryUpdate), start
	for end < len(buckets)*3 && end-start < 256 {
		bucket := buckets[end/3]
		step := [...]int64{60, 300, 3600}[end%3]
		row := [...]any{bucket.Node, bucket.Series, step, time.Unix(bucket.T, 0).Truncate(time.Duration(step) * time.Second).Unix(), bucket.Total, bucket.Peak, bucket.Samples}
		size := len("(?,?,?,?,?,?,?),") + historyArgsBytes(row[:])
		if bytes+size > 1<<20 {
			if end == start {
				return "", nil, start, errors.New("history row exceeds batch limit")
			}
			break
		}
		if end > start {
			values.WriteByte(',')
		}
		values.WriteString("(?,?,?,?,?,?,?)")
		args = append(args, row[:]...)
		bytes += size
		end++
	}
	return mysqlHistoryInsert + values.String() + mysqlHistoryUpdate, args, end, nil
}

func (s *sqliteStore) Query(ctx context.Context, node, series string, stepSeconds int, start, end int64) ([]Point, error) {
	lease, err := s.db.Acquire(ctx, storage.Reader)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	ctx = lease.Context()
	rows, err := lease.QueryContext(ctx, `SELECT t, total / samples, peak FROM buckets WHERE node_id=? AND metric=? AND step=? AND t>=? AND t<=? ORDER BY t`, node, series, stepSeconds, start, end)
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
	lease, err := s.db.Acquire(ctx, storage.Reader)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	ctx = lease.Context()
	if lease.Dialect() == storage.MySQL {
		tx, err := lease.BeginRead(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		var result []string
		var after string
		first := true
		for {
			var metric string
			var err error
			if first {
				err = tx.QueryRowContext(ctx, `SELECT metric FROM buckets WHERE node_id=? ORDER BY metric LIMIT 1`, node).Scan(&metric)
			} else {
				err = tx.QueryRowContext(ctx, `SELECT metric FROM buckets WHERE node_id=? AND metric>? ORDER BY metric LIMIT 1`, node, after).Scan(&metric)
			}
			if errors.Is(err, sql.ErrNoRows) {
				return result, nil
			}
			if err != nil {
				return nil, err
			}
			result = append(result, metric)
			after = metric
			first = false
		}
	}
	rows, err := lease.QueryContext(ctx, historySeriesQuery, node)
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
	lease, err := s.db.Acquire(ctx, storage.Reader)
	if err != nil {
		return err
	}
	defer lease.Close()
	ctx = lease.Context()
	if lease.Dialect() == storage.MySQL {
		tx, err := lease.BeginRead(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var after string
		first := true
		for {
			var id string
			var err error
			if first {
				err = tx.QueryRowContext(ctx, `SELECT node_id FROM buckets ORDER BY node_id LIMIT 1`).Scan(&id)
			} else {
				err = tx.QueryRowContext(ctx, `SELECT node_id FROM buckets WHERE node_id>? ORDER BY node_id LIMIT 1`, after).Scan(&id)
			}
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			visit(id)
			after = id
			first = false
		}
	}
	rows, err := lease.QueryContext(ctx, historyNodesQuery)
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

// 每批使用独立短写事务，复合主键限制每次删除的工作量。
func (s *sqliteStore) deleteBatches(ctx context.Context, yield func(), node, query string, args ...any) (removed int64, err error) {
	for {
		n, err := s.deleteHistoryBatch(ctx, node, query, args...)
		removed += n
		if err != nil {
			return removed, err
		}
		yield()
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if n < cleanupBatchSize {
			return removed, nil
		}
	}
}

func (s *sqliteStore) deleteHistoryBatch(ctx context.Context, node, query string, args ...any) (int64, error) {
	if node != "" && s.nodes != nil {
		main, err := s.nodes.Acquire(ctx, storage.Reader)
		if err != nil {
			return 0, err
		}
		defer main.Close()
		ctx = main.Context()
		snapshot, err := main.BeginRead(ctx)
		if err != nil {
			return 0, err
		}
		defer snapshot.Rollback()
		var deleted int
		if err := snapshot.QueryRowContext(ctx, `SELECT count(*) FROM node_deletions WHERE node_id=? AND NOT EXISTS (SELECT 1 FROM nodes WHERE id=?)`, node, node).Scan(&deleted); err != nil {
			return 0, err
		}
		if deleted == 0 {
			return 0, nil
		}
	}
	lease, err := s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return 0, err
	}
	defer lease.Close()
	return deleteBatch(lease.Context(), lease, query, args...)
}

func deleteBatch(ctx context.Context, lease *storage.Lease, query string, args ...any) (int64, error) {
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	keys, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var primary []any
	var placeholders []string
	for keys.Next() {
		var node, metric string
		var step, t int64
		if err = keys.Scan(&node, &metric, &step, &t); err != nil {
			break
		}
		primary = append(primary, node, metric, step, t)
		placeholders = append(placeholders, "(?,?,?,?)")
	}
	err = errors.Join(err, keys.Err(), keys.Close())
	if err != nil {
		return 0, err
	}
	if len(primary) == 0 {
		return 0, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM buckets WHERE (node_id,metric,step,t) IN (`+strings.Join(placeholders, ",")+")", primary...)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *sqliteStore) DeleteNode(ctx context.Context, node string, yield func()) (int64, error) {
	return s.deleteBatches(ctx, yield, node, `SELECT node_id,metric,step,t FROM buckets WHERE node_id=? ORDER BY node_id,metric,step,t LIMIT ?`, node, cleanupBatchSize)
}

func (s *sqliteStore) DeleteExpired(ctx context.Context, now time.Time, yield func()) (int64, error) {
	var removed int64
	for _, tier := range []struct {
		step int
		keep time.Duration
	}{{60, Retention1m}, {300, Retention5m}, {3600, Retention1h}} {
		n, err := s.deleteBatches(ctx, yield, "", `SELECT node_id,metric,step,t FROM buckets WHERE step=? AND t<? ORDER BY node_id,metric,step,t LIMIT ?`, tier.step, now.Add(-tier.keep).Truncate(time.Duration(tier.step)*time.Second).Unix(), cleanupBatchSize)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (s *sqliteStore) DatabaseStats() (writer, reader sql.DBStats, ok bool) {
	return s.db.Stats()
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}
