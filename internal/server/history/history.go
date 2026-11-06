package history

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/logging"
)

const (
	CurrentSchemaVersion = 1
	SampleBufferSize     = 4096
	FlushInterval        = time.Minute
	maxPendingBuckets    = 8192
	maxDiskPaths         = 16
)

type Options struct {
	Now    func() time.Time
	Logger *slog.Logger
	// ObserveFlush 仅在启用指标时设置；条目数为本次尝试写入的各层级聚合行数。
	ObserveFlush func(time.Duration, int)
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
	store          Store
	nodes          *sql.DB
	samples        chan Sample
	retention      chan retentionRequest
	stop           chan context.Context
	done           chan struct{}
	cancel         context.CancelFunc
	once           sync.Once
	err            error // published by closing done
	now            func() time.Time
	logger         *slog.Logger
	pending        map[bucketKey]map[string]aggregate // owned by run
	dropped        atomic.Uint64
	observeFlush   func(time.Duration, int)
	paths          map[string]map[string]struct{} // owned by run; one entry per observed node
	seriesRejected atomic.Uint64
}

// Open 创建 metrics.db，使用自己的迁移台账和只读查询池。
// 主库连接池仅用于清理孤立历史记录前读取现有节点 ID。
func Open(ctx context.Context, dataDir string, nodes *sql.DB, opts Options) (*Service, error) {
	store, err := openSQLiteStore(ctx, filepath.Join(dataDir, "metrics.db"))
	if err != nil {
		return nil, err
	}
	return newService(store, nodes, opts), nil
}

// newService 在存储就绪后启动采样、刷写和清理任务。
func newService(store Store, nodes *sql.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	workCtx, cancel := context.WithCancel(context.Background())
	s := &Service{store: store, nodes: nodes, samples: make(chan Sample, SampleBufferSize), retention: make(chan retentionRequest), stop: make(chan context.Context, 1), done: make(chan struct{}), cancel: cancel, now: opts.Now, logger: opts.Logger, pending: make(map[bucketKey]map[string]aggregate), paths: make(map[string]map[string]struct{}), observeFlush: opts.ObserveFlush}
	go s.run(workCtx)
	return s
}

// Samples 永不关闭，因此即使迟到的生产者也不会在关闭时 panic。
// 生产者必须使用非阻塞发送，并自己维护丢弃计数。
func (s *Service) Samples() chan<- Sample { return s.samples }
func (s *Service) DroppedBuckets() uint64 { return s.dropped.Load() }
func (s *Service) SeriesRejected() uint64 { return s.seriesRejected.Load() }

// DatabaseStats 仅在抓取指标时读取两个连接池的快照。
func (s *Service) DatabaseStats() (writer, reader sql.DBStats, ok bool) {
	if s.store == nil {
		return writer, reader, false
	}
	return s.store.DatabaseStats()
}

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
		if path, partitioned := diskSeriesPath(metric); partitioned && !s.admitPath(sample.NodeID, path) {
			s.seriesRejected.Add(1)
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

// diskSeriesPath splits only the first colon; paths may themselves contain colons.
func diskSeriesPath(metric string) (string, bool) {
	base, path, found := strings.Cut(metric, ":")
	return path, found && (base == "disk_used_percent" || base == "inode_used_percent")
}

// 只有 run 调用 admitPath。首次从存储读取已有序列，失败时拒绝分区样本，
// 下一个样本到来时重试。
func (s *Service) admitPath(node, path string) bool {
	paths, loaded := s.paths[node]
	if !loaded {
		paths = make(map[string]struct{})
		if s.store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			series, err := s.store.Series(ctx, node)
			if err != nil {
				return false
			}
			for _, metric := range series {
				if known, ok := diskSeriesPath(metric); ok {
					paths[known] = struct{}{}
				}
			}
		}
		if s.paths == nil {
			s.paths = make(map[string]map[string]struct{})
		}
		s.paths[node] = paths
	}
	if _, known := paths[path]; known {
		return true
	}
	if len(paths) >= maxDiskPaths {
		return false
	}
	paths[path] = struct{}{}
	return true
}

func (s *Service) drain() {
	// 只处理当前队列长度，避免持续到来的样本阻碍定期清理或关闭。
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

// flush 将每分钟的精确总和与样本数交给存储，原子合并到所有聚合层级。
// 提交成功后才释放待写状态，因此失败时可以重试。
func (s *Service) flush(ctx context.Context, now time.Time, all bool) error {
	var started time.Time
	if s.observeFlush != nil {
		started = time.Now()
	}
	entries := 0
	var buckets []Bucket
	keys := make([]bucketKey, 0, len(s.pending))
	for key := range s.pending {
		if all || key.t < now.Truncate(time.Minute).Unix() {
			keys = append(keys, key)
			for metric, a := range s.pending[key] {
				buckets = append(buckets, Bucket{Node: key.node, Series: metric, T: key.t, Total: a.total, Peak: a.peak, Samples: a.samples})
			}
		}
	}
	if len(keys) == 0 {
		return ctx.Err()
	}
	if s.observeFlush != nil {
		defer func() { s.observeFlush(time.Since(started), entries) }()
	}
	var err error
	entries, err = s.store.Write(ctx, buckets)
	if err != nil {
		return err
	}
	for _, key := range keys {
		delete(s.pending, key)
	}
	return nil
}

// Cleanup 与刷写串行化，并从所有层级和待处理内存中清除已删除的节点。
// 它从不在主数据库上加写锁。
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
	for id := range s.paths {
		candidates[id] = true
	}
	err := s.store.NodeIDs(ctx, func(id string) {
		candidates[id] = true
		s.drain()
	})
	if err != nil {
		return 0, err
	}
	// 在读取活跃 ID 之前冻结候选。排空可能添加待处理节点，
	// 但在这份存在性快照之后，它们不可能成为孤儿候选。
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
	for id := range candidates {
		n, err := s.store.DeleteNode(ctx, id, s.drain)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	n, err := s.store.DeleteExpired(ctx, now, s.drain)
	removed += n
	if err != nil {
		return removed, err
	}
	for key := range s.pending {
		if candidates[key.node] || key.t < now.Add(-Retention1m).Truncate(time.Minute).Unix() {
			delete(s.pending, key)
		}
	}
	for id := range candidates {
		delete(s.paths, id)
	}
	return removed, s.flush(ctx, now, false)
}

// Shutdown 排空样本队列，写入包括当前未结束分钟在内的全部数据。
// 调用方提供总时限；到期时也会中断正在进行的写入。
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

// CloseContext 在调用方共享的关闭时限内停止写入任务，并关闭历史存储。
func (s *Service) CloseContext(ctx context.Context) error {
	err := s.Shutdown(ctx)
	return errors.Join(err, s.store.Close())
}
