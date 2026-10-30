package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/storage"
)

const (
	CurrentSchemaVersion = 1
	SampleBufferSize     = 4096
	FlushInterval        = time.Minute
	maxPendingBuckets    = 8192
	cleanupBatchSize     = 1000
)

// Seek to the next primary-key node prefix instead of visiting every bucket
// for DISTINCT. Work scales with node count, irrespective of retained history.
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

type Options struct {
	Now    func() time.Time
	Logger *slog.Logger
}

type bucketKey struct {
	node string
	t    int64
}
type aggregate struct {
	total, peak float64
	samples     int64
}
type retentionRequest struct {
	ctx    context.Context
	now    time.Time
	result chan retentionResult
}
type retentionResult struct {
	removed int64
	err     error
}

type Service struct {
	db, reader, nodes *sql.DB
	samples           chan Sample
	retention         chan retentionRequest
	stop              chan context.Context
	done              chan struct{}
	cancel            context.CancelFunc
	once              sync.Once
	err               error // published by closing done
	now               func() time.Time
	logger            *slog.Logger
	pending           map[bucketKey]map[string]aggregate // owned by run
	dropped           atomic.Uint64
}

// OpenDatabase initializes only the trusted metrics schema, without starting a
// writer or retention worker. Offline restore uses it for its private staging.
func OpenDatabase(ctx context.Context, path string) (*sql.DB, error) {
	return storage.OpenWithMigrations(ctx, path, []storage.Migration{{Version: CurrentSchemaVersion, SQL: schema}})
}

// Open creates metrics.db with its own migration ledger and read-only query
// pool. The main pool is used only to read node IDs during orphan retention.
func Open(ctx context.Context, dataDir string, nodes *sql.DB, opts Options) (*Service, error) {
	path := filepath.Join(dataDir, "metrics.db")
	db, err := OpenDatabase(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open metric history: %w", err)
	}
	reader, err := storage.OpenReader(ctx, path)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open metric history reader: %w", err)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	workCtx, cancel := context.WithCancel(context.Background())
	s := &Service{db: db, reader: reader, nodes: nodes, samples: make(chan Sample, SampleBufferSize), retention: make(chan retentionRequest), stop: make(chan context.Context, 1), done: make(chan struct{}), cancel: cancel, now: opts.Now, logger: opts.Logger, pending: make(map[bucketKey]map[string]aggregate)}
	go s.run(workCtx)
	return s, nil
}

// Samples is never closed, so even a late producer cannot panic at shutdown.
// Producers must use a non-blocking send and own their drop counter.
func (s *Service) Samples() chan<- Sample { return s.samples }
func (s *Service) DroppedBuckets() uint64 { return s.dropped.Load() }

func (s *Service) add(sample Sample) {
	if sample.NodeID == "" || sample.ReceivedAt.IsZero() || len(sample.Values) == 0 {
		return
	}
	key := bucketKey{sample.NodeID, sample.ReceivedAt.Truncate(time.Minute).Unix()}
	values := s.pending[key]
	if values == nil {
		if len(s.pending) >= maxPendingBuckets {
			s.dropped.Add(1)
			return
		}
		values = make(map[string]aggregate)
		s.pending[key] = values
	}
	for metric, value := range sample.Values {
		if !validValue(metric, value) {
			continue
		}
		a := values[metric]
		if a.samples == math.MaxInt64 {
			continue
		}
		a.total += value
		a.peak = max(a.peak, value)
		a.samples++
		values[metric] = a
	}
}

func (s *Service) drain() {
	// A finite snapshot prevents live producers from starving retention/shutdown.
	for remaining := len(s.samples); remaining > 0; remaining-- {
		select {
		case sample := <-s.samples:
			s.add(sample)
		default:
			return
		}
	}
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	defer s.cancel()
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case sample := <-s.samples:
			s.add(sample)
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.flush(flushCtx, s.now(), false)
			cancel()
			if err != nil {
				if s.logger != nil {
					s.logger.LogAttrs(ctx, slog.LevelWarn, "metric_history_flush_failed", logging.ErrorAttrs(err, "flush_failed")...)
				}
			}
		case request := <-s.retention:
			s.drain()
			removed, err := s.cleanup(request.ctx, request.now)
			request.result <- retentionResult{removed, err}
		case shutdownCtx := <-s.stop:
			s.drain()
			s.err = s.flush(shutdownCtx, s.now(), true)
			return
		case <-ctx.Done():
			s.err = ctx.Err()
			return
		}
	}
}

// flush merges each minute's exact sums/counts into all tiers in one transaction.
// Additive upserts preserve a partially flushed minute across server restarts.
// Pending state is released only after commit, so a failed flush can be retried.
func (s *Service) flush(ctx context.Context, now time.Time, all bool) error {
	keys := make([]bucketKey, 0, len(s.pending))
	for key := range s.pending {
		if all || key.t < now.Truncate(time.Minute).Unix() {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ctx.Err()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES(?,?,?,?,?,?,?) ON CONFLICT(node_id,metric,step,t) DO UPDATE SET total=total+excluded.total, peak=max(peak,excluded.peak), samples=samples+excluded.samples`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, key := range keys {
		for metric, a := range s.pending[key] {
			for _, step := range []int64{60, 300, 3600} {
				t := time.Unix(key.t, 0).Truncate(time.Duration(step) * time.Second).Unix()
				if _, err := statement.ExecContext(ctx, key.node, metric, step, t, a.total, a.peak, a.samples); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, key := range keys {
		delete(s.pending, key)
	}
	return nil
}

// Cleanup is serialized with flushing and purges deleted nodes from all tiers
// and pending memory. It never takes a writer lock on the main database.
func (s *Service) Cleanup(ctx context.Context, now time.Time) (int64, error) {
	result := make(chan retentionResult, 1)
	select {
	case s.retention <- retentionRequest{ctx, now, result}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.done:
		return 0, errors.New("metric history is stopped")
	}
	select {
	case r := <-result:
		return r.removed, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (s *Service) cleanup(ctx context.Context, now time.Time) (int64, error) {
	candidates := make(map[string]bool)
	for key := range s.pending {
		candidates[key.node] = true
	}
	rows, err := s.db.QueryContext(ctx, historyNodesQuery)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		candidates[id] = true
		s.drain()
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	// Freeze candidates before reading live IDs. Draining may add pending nodes,
	// but they cannot become orphan candidates after this existence snapshot.
	if s.nodes != nil {
		rows, err := s.nodes.QueryContext(ctx, `SELECT id FROM nodes`)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return 0, err
			}
			delete(candidates, id)
			s.drain()
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return 0, err
		}
	} else {
		clear(candidates)
	}
	var removed int64
	// Every Exec is its own short autocommit transaction. The table is WITHOUT
	// ROWID, so select at most one batch of composite primary keys per delete.
	deleteBatches := func(query string, args ...any) error {
		for {
			result, err := s.db.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			removed += n
			s.drain()
			if err := ctx.Err(); err != nil {
				return err
			}
			if n < cleanupBatchSize {
				return nil
			}
		}
	}
	for id := range candidates {
		if err := deleteBatches(`DELETE FROM buckets WHERE (node_id,metric,step,t) IN (SELECT node_id,metric,step,t FROM buckets WHERE node_id=? LIMIT ?)`, id, cleanupBatchSize); err != nil {
			return removed, err
		}
	}
	for _, tier := range []struct {
		step int
		keep time.Duration
	}{{60, Retention1m}, {300, Retention5m}, {3600, Retention1h}} {
		if err := deleteBatches(`DELETE FROM buckets WHERE (node_id,metric,step,t) IN (SELECT node_id,metric,step,t FROM buckets WHERE step=? AND t<? LIMIT ?)`, tier.step, now.Add(-tier.keep).Truncate(time.Duration(tier.step)*time.Second).Unix(), cleanupBatchSize); err != nil {
			return removed, err
		}
	}
	for key := range s.pending {
		if candidates[key.node] || key.t < now.Add(-Retention1m).Truncate(time.Minute).Unix() {
			delete(s.pending, key)
		}
	}
	return removed, s.flush(ctx, now, false)
}

// Shutdown drains queued samples and flushes even the incomplete minute. The
// caller supplies the total budget; expiry also interrupts an ongoing flush.
func (s *Service) Shutdown(ctx context.Context) error {
	s.once.Do(func() { s.stop <- ctx })
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		s.cancel()
		return ctx.Err()
	}
}

func (s *Service) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.CloseContext(ctx)
}

// CloseContext shuts down the writer and closes both history database pools
// within the caller's shared shutdown budget.
func (s *Service) CloseContext(ctx context.Context) error {
	err := s.Shutdown(ctx)
	return errors.Join(err, s.reader.Close(), s.db.Close())
}
