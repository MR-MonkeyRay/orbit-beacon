package history

import (
	"context"
	"errors"
	"strings"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

// 每份删除集合只属于读取它的 main 世代，不把旧分支的墓碑带到新权威。
func (s *Service) invalidateGenerations() {
	if s.nodes != nil && s.mainGeneration != s.nodes.Generation() {
		s.mainGeneration = s.nodes.Generation()
		s.deleted = nil
	}
	if db := s.Database(); db != nil && s.pathGeneration != db.Generation() {
		s.pathGeneration = db.Generation()
		clear(s.paths)
		s.seriesRetryAt = time.Time{}
	}
}

func readDeletions(ctx context.Context, tx *storage.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT node_id FROM node_deletions WHERE NOT EXISTS (SELECT 1 FROM nodes WHERE nodes.id=node_deletions.node_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deleted := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		deleted[id] = true
	}
	return deleted, rows.Err()
}

// 调用方在 history SQL 及内存发布完成后释放 main lease，锁顺序固定为 main → history。
func (s *Service) deletionSnapshot(ctx context.Context) (*storage.Lease, error) {
	s.invalidateGenerations()
	if s.nodes == nil {
		return nil, nil
	}
	lease, err := s.nodes.Acquire(ctx, storage.Reader)
	if err != nil {
		return nil, err
	}
	tx, err := lease.BeginRead(lease.Context())
	if err != nil {
		lease.Close()
		return nil, err
	}
	deleted, err := readDeletions(lease.Context(), tx)
	err = errors.Join(err, tx.Rollback())
	if err != nil {
		lease.Close()
		return nil, err
	}
	s.deleted, s.mainGeneration = deleted, lease.Generation()
	return lease, nil
}

func (s *Service) refreshDeletions(ctx context.Context) error {
	lease, err := s.deletionSnapshot(ctx)
	if lease != nil {
		lease.Close()
	}
	return err
}

func (s *Service) purgeDeleted(ctx context.Context, yield func()) (int64, error) {
	if err := s.refreshDeletions(ctx); err != nil {
		return 0, err
	}
	var removed int64
	for id := range s.deleted {
		previousPaths := s.paths[id]
		delete(s.paths, id)
		// 暂存删除前的桶；批次间排空的新样本必须进入独立 pending。
		var previous map[bucketKey]map[string]aggregate
		for key, values := range s.pending {
			if key.node == id {
				if previous == nil {
					previous = make(map[bucketKey]map[string]aggregate)
				}
				previous[key] = values
				delete(s.pending, key)
			}
		}
		n, err := s.store.DeleteNode(ctx, id, yield)
		removed += n
		if err != nil {
			for key, values := range previous {
				if s.pending[key] == nil {
					s.pending[key] = values
					continue
				}
				for metric, value := range values {
					current := s.pending[key][metric]
					current.total += value.total
					current.peak = max(current.peak, value.peak)
					current.samples += value.samples
					s.pending[key][metric] = current
				}
			}
			if len(previousPaths) > 0 {
				if s.paths[id] == nil {
					s.paths[id] = previousPaths
				} else {
					for path := range previousPaths {
						s.paths[id][path] = struct{}{}
					}
				}
			}
			return removed, err
		}
		// SQL 删除成功即废止旧桶；复活和后续快照失败都不能恢复它们。
		kept := s.inflight[:0]
		for _, bucket := range s.inflight {
			if bucket.Node != id {
				kept = append(kept, bucket)
			}
		}
		s.inflight = kept
		lease, err := s.deletionSnapshot(ctx)
		if lease != nil {
			lease.Close()
		}
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (s *Service) prepareHistoryGeneration(ctx context.Context, now time.Time) error {
	s.invalidateGenerations()
	db := s.Database()
	if db == nil || db.Generation() == s.historyGeneration {
		return nil
	}
	if store, ok := s.store.(*sqliteStore); ok && store.unresolved != nil {
		return storage.ErrCommitUnknown
	}
	generation := db.Generation()
	if _, err := s.store.DeleteExpired(ctx, now, func() {}); err != nil {
		return err
	}
	if _, err := s.purgeDeleted(ctx, func() {}); err != nil {
		return err
	}
	if db.Generation() != generation {
		return storage.ErrUnavailable
	}
	s.historyGeneration = generation
	return nil
}

func (s *Service) cleanupTombstones(ctx context.Context, now time.Time) error {
	if s.nodes == nil {
		return nil
	}
	// deleted_at < 此整点，等价于 truncate_hour(deleted_at)+Retention1h+1h <= now。
	cutoff := timekey.Format(now.Truncate(time.Hour).Add(-Retention1h))
	for {
		reader, err := s.nodes.Acquire(ctx, storage.Reader)
		if err != nil {
			return err
		}
		ids, err := expiredTombstones(reader, cutoff)
		reader.Close()
		if err != nil || len(ids) == 0 {
			return err
		}
		lease, err := s.nodes.Acquire(ctx, storage.Writer)
		if err != nil {
			return err
		}
		err = deleteTombstoneBatch(lease, cutoff, ids)
		lease.Close()
		if err != nil || len(ids) < cleanupBatchSize {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func expiredTombstones(lease *storage.Lease, cutoff string) ([]string, error) {
	rows, err := lease.QueryContext(lease.Context(), `SELECT node_id FROM node_deletions WHERE deleted_at<? ORDER BY node_id LIMIT ?`, cutoff, cleanupBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func deleteTombstoneBatch(lease *storage.Lease, cutoff string, ids []string) error {
	ctx := lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	args := make([]any, 0, len(ids)+1)
	args = append(args, cutoff)
	for _, id := range ids {
		args = append(args, id)
	}
	query := `DELETE FROM node_deletions WHERE deleted_at<? AND node_id IN (` + strings.Repeat("?,", len(ids)-1) + `?)`
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}
