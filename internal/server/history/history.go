package history

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/storage"
)

const (
	CurrentSchemaVersion  = storage.HistorySchemaVersion
	SampleBufferSize      = 4096
	FlushInterval         = time.Minute
	maxPendingBuckets     = 8192
	maxDiskPaths          = 16
	maxFlushKeys          = 128
	maxFlushBuckets       = 512
	maxHistoryRowBytes    = 5 << 10
	maxHistoryTxBytes     = 64 << 20
	maxHistoryMetricBytes = 1043
	maxHistoryNodeBytes   = 64
	historyTxHeaderBytes  = 1024
	// 远端存储不可用时，每个节点的首个样本都会等待超时；退避避免写入循环被占满。
	seriesRetryDelay = 30 * time.Second
)

var errHistoryBatchInvalid = errors.New("history_batch_invalid")

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
	store             Store
	nodes             *storage.Database
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
	inflight          []Bucket                           // 提交未明的不可变批次，不与新样本混合
	dropped           atomic.Uint64
	observeFlush      func(time.Duration, int)
	paths             map[string]map[string]struct{} // owned by run; one entry per observed node
	seriesRejected    atomic.Uint64
	seriesRetryAt     time.Time // owned by run
	deleted           map[string]bool
	mainGeneration    uint64
	pathGeneration    uint64
	historyGeneration uint64
}

// Open 创建独立历史域；清理时先固定主库，再访问历史库。
func Open(ctx context.Context, dataDir string, nodes *storage.Database, opts Options) (*Service, error) {
	store, err := openSQLiteStore(ctx, filepath.Join(dataDir, "metrics.db"))
	if err != nil {
		return nil, err
	}
	return newService(store, nodes, opts), nil
}

// OpenWithDatabase 接管历史句柄；Shutdown 排空采样后 Close 才关闭连接池。
func OpenWithDatabase(database, nodes *storage.Database, opts Options) *Service {
	return newService(&sqliteStore{db: database}, nodes, opts)
}

// newService 在存储就绪后启动采样、刷写和清理任务。
func newService(store Store, nodes *storage.Database, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	workCtx, cancel := context.WithCancel(context.Background())
	s := &Service{store: store, nodes: nodes, samples: make(chan Sample, SampleBufferSize), retention: make(chan retentionRequest), stop: make(chan context.Context, 1), done: make(chan struct{}), cancel: cancel, now: opts.Now, logger: opts.Logger, pending: make(map[bucketKey]map[string]aggregate), paths: make(map[string]map[string]struct{}), observeFlush: opts.ObserveFlush}
	if sqlStore, ok := store.(*sqliteStore); ok {
		sqlStore.nodes = nodes
		s.pathGeneration = sqlStore.db.Generation()
		s.historyGeneration = s.pathGeneration
	}
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

// Database 返回当前 SQL 历史域，不构造空本地替代库。
func (s *Service) Database() *storage.Database {
	if store, ok := s.store.(*sqliteStore); ok {
		return store.db
	}
	return nil
}

func (s *Service) add(sample Sample) {
	if sample.NodeID == "" || sample.ReceivedAt.IsZero() || len(sample.Values) == 0 {
		return
	}
	s.invalidateGenerations()
	if s.deleted[sample.NodeID] {
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
// 退避结束后的下一个样本再重试。
func (s *Service) admitPath(node, path string) bool {
	s.invalidateGenerations()
	paths, loaded := s.paths[node]
	if !loaded {
		paths = make(map[string]struct{})
		if s.store != nil {
			if s.now().Before(s.seriesRetryAt) {
				return false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.prepareHistoryGeneration(ctx, s.now()); err != nil {
				return false
			}
			if s.deleted[node] {
				return false
			}
			series, err := s.store.Series(ctx, node)
			if err != nil {
				s.seriesRetryAt = s.now().Add(seriesRetryDelay)
				return false
			}
			for _, metric := range series {
				if known, ok := diskSeriesPath(metric); ok {
					paths[known] = struct{}{}
				}
			}
			for key, values := range s.pending {
				if key.node == node {
					for metric := range values {
						if known, ok := diskSeriesPath(metric); ok {
							paths[known] = struct{}{}
						}
					}
				}
			}
			for _, bucket := range s.inflight {
				if bucket.Node == node {
					if known, ok := diskSeriesPath(bucket.Series); ok {
						paths[known] = struct{}{}
					}
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
			s.err = s.flushShutdown(shutdownCtx)
			return
		case <-ctx.Done():
			s.err = ctx.Err()
			return
		}
	}
}

// 关闭时只继续核对未知提交的原批次；等待期间不持有 SQL 资源，也不延长调用方时限。
func (s *Service) flushShutdown(ctx context.Context) error {
	var rejected error
	var retry *time.Timer
	for {
		err := s.flush(ctx, s.now(), true)
		if errors.Is(err, errHistoryBatchInvalid) {
			rejected = errHistoryBatchInvalid
		}
		if !errors.Is(err, storage.ErrCommitUnknown) || len(s.inflight) == 0 {
			return errors.Join(rejected, err)
		}
		if ctx.Err() != nil {
			return errors.Join(rejected, err, ctx.Err())
		}
		if retry == nil {
			retry = time.NewTimer(100 * time.Millisecond)
			defer retry.Stop()
		} else {
			retry.Reset(100 * time.Millisecond)
		}
		select {
		case <-retry.C:
		case <-ctx.Done():
			return errors.Join(rejected, err, ctx.Err())
		}
		if ctx.Err() != nil {
			return errors.Join(rejected, err, ctx.Err())
		}
	}
}

// flush 按分钟、节点、指标顺序逐批排空；一个节点分钟的三档聚合始终在同一事务。
func (s *Service) flush(ctx context.Context, now time.Time, all bool) error {
	if len(s.inflight) > 0 {
		if err := s.writeInflight(ctx, now); err != nil {
			return err
		}
		s.drain()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := s.prepareHistoryGeneration(ctx, now); err != nil {
		return err
	}
	if err := s.refreshDeletions(ctx); err != nil {
		return err
	}
	keys := make([]bucketKey, 0, len(s.pending))
	for key := range s.pending {
		if key.t < now.Add(-Retention1m).Truncate(time.Minute).Unix() {
			delete(s.pending, key)
			continue
		}
		if s.deleted[key.node] {
			continue
		}
		if all || key.t < now.Truncate(time.Minute).Unix() {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b bucketKey) int {
		if order := cmp.Compare(a.t, b.t); order != 0 {
			return order
		}
		return cmp.Compare(a.node, b.node)
	})
	var rejected error
	for next := 0; next < len(keys); {
		buckets := make([]Bucket, 0, maxFlushBuckets)
		batchKeys := 0
		for next < len(keys) && batchKeys < maxFlushKeys {
			key := keys[next]
			values := s.pending[key]
			if !validHistoryGroup(key, values) {
				delete(s.pending, key)
				s.dropped.Add(1)
				rejected = errHistoryBatchInvalid
				next++
				continue
			}
			if len(buckets)+len(values) > maxFlushBuckets {
				break
			}
			start := len(buckets)
			for metric, a := range values {
				buckets = append(buckets, Bucket{Node: key.node, Series: metric, T: key.t, Total: a.total, Peak: a.peak, Samples: a.samples})
			}
			slices.SortFunc(buckets[start:], func(a, b Bucket) int { return cmp.Compare(a.Series, b.Series) })
			delete(s.pending, key)
			batchKeys++
			next++
		}
		if len(buckets) > 0 {
			// 每个桶最多产生三条 5 KiB 行日志，另为事务头保留空间。
			if historyTxHeaderBytes+3*len(buckets)*maxHistoryRowBytes > maxHistoryTxBytes {
				s.restorePending(buckets)
				return errors.Join(rejected, errHistoryBatchInvalid)
			}
			s.inflight = buckets
			if err := s.writeInflight(ctx, now); err != nil {
				if !errors.Is(err, storage.ErrCommitUnknown) {
					s.restorePending(s.inflight)
					s.inflight = nil
				}
				return errors.Join(rejected, err)
			}
			s.drain()
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(rejected, err)
		}
	}
	return errors.Join(rejected, ctx.Err())
}

// 文本在主键和 after image 中各出现一次，十六进制编码共占四倍字节。
// 512 字节覆盖两份数组、类型标签和全部 int64/double 的最坏十进制长度。
func validHistoryGroup(key bucketKey, values map[string]aggregate) bool {
	if len(values) > maxFlushBuckets || len(key.node) > maxHistoryNodeBytes || !utf8.ValidString(key.node) {
		return false
	}
	for metric, a := range values {
		if len(metric) > maxHistoryMetricBytes || !utf8.ValidString(metric) || 512+4*(len(key.node)+len(metric)) > maxHistoryRowBytes || a.samples <= 0 || math.IsNaN(a.total) || math.IsInf(a.total, 0) || math.IsNaN(a.peak) || math.IsInf(a.peak, 0) {
			return false
		}
	}
	return true
}

func (s *Service) restorePending(buckets []Bucket) {
	for _, bucket := range buckets {
		key := bucketKey{bucket.Node, bucket.T}
		if s.pending[key] == nil {
			s.pending[key] = make(map[string]aggregate)
		}
		a := s.pending[key][bucket.Series]
		a.total += bucket.Total
		a.peak = max(a.peak, bucket.Peak)
		a.samples += bucket.Samples
		s.pending[key][bucket.Series] = a
	}
}

// Store 对未解析提交保留原事务收据；只允许携带完全相同的不可变批次重入。
func (s *Service) writeInflight(ctx context.Context, now time.Time) error {
	rows := 3 * len(s.inflight)
	if observer := s.observeFlush; observer != nil {
		started := time.Now()
		defer func() { observer(time.Since(started), rows) }()
	}
	if store, ok := s.store.(*sqliteStore); ok && store.unresolved != nil {
		committed, err := store.resolveInflight(ctx)
		if err != nil {
			return err
		}
		if !committed {
			s.restorePending(s.inflight)
		}
		s.inflight = nil
		return nil
	}
	if err := s.prepareHistoryGeneration(ctx, now); err != nil {
		return err
	}
	lease, err := s.deletionSnapshot(ctx)
	if err != nil {
		return err
	}
	if lease != nil {
		defer lease.Close()
		ctx = lease.Context()
	}
	if db := s.Database(); db != nil {
		history, err := db.Acquire(ctx, storage.Writer)
		if err != nil {
			return err
		}
		defer history.Close()
		if history.Generation() != s.historyGeneration {
			return storage.ErrUnavailable
		}
		ctx = history.Context()
	}
	kept := s.inflight[:0]
	for _, bucket := range s.inflight {
		if bucket.T < now.Add(-Retention1m).Truncate(time.Minute).Unix() {
			continue
		}
		if s.deleted[bucket.Node] {
			s.restorePending([]Bucket{bucket})
		} else {
			kept = append(kept, bucket)
		}
	}
	s.inflight = kept
	if len(s.inflight) == 0 {
		return nil
	}
	rows, err = s.store.Write(ctx, s.inflight)
	if err == nil {
		s.inflight = nil
	}
	return err
}

// Cleanup 串行处理 TTL、有效墓碑及刷写；每个跨域删除批次固定 main → history。
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
	if store, ok := s.store.(*sqliteStore); ok && store.unresolved != nil {
		committed, err := store.resolveInflight(ctx)
		if err != nil {
			return 0, err
		}
		if !committed {
			s.restorePending(s.inflight)
		}
		s.inflight = nil
	}
	s.invalidateGenerations()
	removed, err := s.store.DeleteExpired(ctx, now, s.drain)
	if err != nil {
		return removed, err
	}
	n, err := s.purgeDeleted(ctx, s.drain)
	removed += n
	if err != nil {
		return removed, err
	}
	for key := range s.pending {
		if key.t < now.Add(-Retention1m).Truncate(time.Minute).Unix() {
			delete(s.pending, key)
		}
	}
	if err := s.cleanupTombstones(ctx, now); err != nil {
		return removed, err
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
