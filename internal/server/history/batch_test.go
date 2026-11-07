package history

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

type batchRecordingStore struct {
	Store
	writes [][]Bucket
	write  func(context.Context, []Bucket) (int, error)
}

func (s *batchRecordingStore) Write(ctx context.Context, buckets []Bucket) (int, error) {
	s.writes = append(s.writes, slices.Clone(buckets))
	if s.write != nil {
		return s.write(ctx, buckets)
	}
	return s.Store.Write(ctx, buckets)
}

func checkHistoryBatches(t *testing.T, writes [][]Bucket) {
	t.Helper()
	seen := make(map[bucketKey]int)
	var previous Bucket
	for batch, buckets := range writes {
		keys := make(map[bucketKey]bool)
		if len(buckets) > 512 {
			t.Fatalf("批次 %d 有 %d 个桶", batch, len(buckets))
		}
		for _, b := range buckets {
			key := bucketKey{b.Node, b.T}
			if owner, ok := seen[key]; ok && owner != batch {
				t.Fatalf("节点分钟被拆分: %+v", key)
			}
			seen[key], keys[key] = batch, true
			if previous.Node != "" && (b.T < previous.T || b.T == previous.T && (b.Node < previous.Node || b.Node == previous.Node && b.Series <= previous.Series)) {
				t.Fatalf("批次顺序不稳定: %+v 后为 %+v", previous, b)
			}
			previous = b
		}
		if len(keys) > 128 {
			t.Fatalf("批次 %d 有 %d 个节点分钟", batch, len(keys))
		}
	}
}

func TestHistoryFlushDrains8192KeysExactlyOnce(t *testing.T) {
	real := openTestSQLiteStore(t)
	store := &batchRecordingStore{Store: real}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	wantTotal, wantSamples := float64(0), int64(0)
	for i := 8191; i >= 0; i-- {
		node := fmt.Sprintf("00000000-0000-4000-8000-%012d", i%128)
		at := now.Add(-time.Duration(1+i/128) * time.Minute)
		values := map[string]float64{
			"cpu_percent": 1, "load1": 2,
			"disk_used_percent:" + strings.Repeat("x", 1024):  3,
			"inode_used_percent:" + strings.Repeat("y", 1024): 4,
		}
		s.add(Sample{node, at, values})
		wantTotal += 10
		wantSamples += 4
	}
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 64 || len(s.pending) != 0 || len(s.inflight) != 0 {
		t.Fatalf("批次数=%d pending=%d inflight=%d", len(store.writes), len(s.pending), len(s.inflight))
	}
	checkHistoryBatches(t, store.writes)
	for _, step := range []int{60, 300, 3600} {
		var total float64
		var samples int64
		if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE step=?", step).Scan(&total, &samples); err != nil {
			t.Fatal(err)
		}
		if total != wantTotal || samples != wantSamples {
			t.Fatalf("step=%d total=%v samples=%d", step, total, samples)
		}
	}
}

func TestHistoryFlushKeepsWholeMaximumDiskGroup(t *testing.T) {
	store := &batchRecordingStore{Store: &fakeStore{}}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	values := map[string]float64{}
	for _, metric := range []string{"cpu_percent", "load1", "load5", "load15", "memory_used_percent", "swap_used_percent", "disk_read_bytes_per_second", "disk_write_bytes_per_second", "disk_busy_percent", "pressure_cpu_percent", "pressure_memory_percent", "pressure_io_percent", "temperature_celsius", "disk_used_percent", "inode_used_percent", "net_rx_bytes_per_second", "net_tx_bytes_per_second"} {
		values[metric] = 1
	}
	for p := range 16 {
		path := fmt.Sprintf("/%04d", p) + strings.Repeat("x", 1019)
		values["disk_used_percent:"+path] = 1
		values["inode_used_percent:"+path] = 1
	}
	for n := range 8192 {
		s.add(Sample{fmt.Sprintf("node-%04d", n), now.Add(-time.Minute), values})
	}
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	checkHistoryBatches(t, store.writes)
	count := 0
	for _, batch := range store.writes {
		if len(batch)%49 != 0 {
			t.Fatalf("49 指标组被拆分: %d", len(batch))
		}
		count += len(batch)
	}
	if len(store.writes) != 820 || count != 8192*49 || len(s.pending) != 0 {
		t.Fatalf("批次数=%d 桶数=%d pending=%d", len(store.writes), count, len(s.pending))
	}
}

func TestHistoryFlushCancellationKeepsUnattemptedKeys(t *testing.T) {
	store := &batchRecordingStore{Store: &fakeStore{}}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	for i := range 300 {
		s.add(Sample{fmt.Sprintf("node-%03d", i), now.Add(-time.Minute), map[string]float64{"cpu_percent": 1}})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.write = func(context.Context, []Bucket) (int, error) {
		cancel()
		return 384, nil
	}
	if err := s.flush(ctx, now, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(store.writes) != 1 || len(s.pending) != 172 {
		t.Fatalf("批次数=%d pending=%d", len(store.writes), len(s.pending))
	}
	store.write = nil
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	checkHistoryBatches(t, store.writes)
	if len(s.pending) != 0 {
		t.Fatalf("未排空 %d 个节点分钟", len(s.pending))
	}
}

func TestHistoryFlushRejectsMalformedGroupWithoutBlockingBacklog(t *testing.T) {
	store := &batchRecordingStore{Store: &fakeStore{}}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Minute).Unix()
	s.pending[bucketKey{"oversized", at}] = make(map[string]aggregate)
	for i := range 513 {
		s.pending[bucketKey{"oversized", at}][fmt.Sprintf("metric-%d", i)] = aggregate{1, 1, 1}
	}
	s.pending[bucketKey{"long", at}] = map[string]aggregate{strings.Repeat("x", 1044): {1, 1, 1}}
	s.pending[bucketKey{"nonfinite", at}] = map[string]aggregate{"cpu_percent": {math.Inf(1), 1, 1}}
	s.pending[bucketKey{"invalid-text", at}] = map[string]aggregate{"\xff": {1, 1, 1}}
	s.add(Sample{"valid", time.Unix(at, 0), map[string]float64{"cpu_percent": 7}})
	if err := s.flush(t.Context(), now, false); !errors.Is(err, errHistoryBatchInvalid) {
		t.Fatalf("错误=%v", err)
	}
	if len(s.pending) != 0 || s.DroppedBuckets() != 4 || len(store.writes) != 1 || len(store.writes[0]) != 1 || store.writes[0][0].Node != "valid" {
		t.Fatalf("pending=%v dropped=%d writes=%v", s.pending, s.DroppedBuckets(), store.writes)
	}
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatalf("畸形组仍在重试: %v", err)
	}
}

func TestHistoryInflightRetryPreservesNewSamplesAfterDefiniteFailure(t *testing.T) {
	store := &batchRecordingStore{Store: &fakeStore{}}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Minute)
	s.add(Sample{"node", at, map[string]float64{"cpu_percent": 20}})
	store.write = func(context.Context, []Bucket) (int, error) { return 3, storage.ErrCommitUnknown }
	if err := s.flush(t.Context(), now, false); !errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatal(err)
	}
	s.add(Sample{"node", at, map[string]float64{"cpu_percent": 30}})
	failure := errors.New("已证明回滚，重试失败")
	store.write = func(context.Context, []Bucket) (int, error) { return 3, failure }
	if err := s.flush(t.Context(), now, false); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	store.write = nil
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 4 || !reflect.DeepEqual(store.writes[0], store.writes[1]) || !reflect.DeepEqual(store.writes[0], store.writes[2]) || store.writes[3][0].Total != 30 || len(s.pending) != 0 || len(s.inflight) != 0 {
		t.Fatalf("批次身份或新样本丢失: %+v", store.writes)
	}
}

func TestHistoryUnknownOutcomeDoesNotDoubleCountRollups(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			real := openTestSQLiteStore(t)
			store := &batchRecordingStore{Store: real}
			s := stoppedStoreService(t, store, nil, Options{})
			now := time.Now().UTC().Truncate(time.Minute)
			at := now.Add(-time.Minute)
			attempts := 0
			store.write = func(ctx context.Context, buckets []Bucket) (int, error) {
				attempts++
				if attempts == 1 {
					if committed {
						if _, err := real.Write(ctx, buckets); err != nil {
							t.Fatal(err)
						}
					}
					return 3 * len(buckets), storage.ErrCommitUnknown
				}
				if attempts == 2 {
					return 3 * len(buckets), storage.ErrCommitUnknown
				}
				if attempts == 3 && committed {
					return 3 * len(buckets), nil
				}
				return real.Write(ctx, buckets)
			}
			s.add(Sample{"node", at, map[string]float64{"cpu_percent": 20}})
			if err := s.flush(t.Context(), now, false); !errors.Is(err, storage.ErrCommitUnknown) {
				t.Fatal(err)
			}
			s.add(Sample{"node", at, map[string]float64{"cpu_percent": 30}})
			if err := s.flush(t.Context(), now, false); !errors.Is(err, storage.ErrCommitUnknown) {
				t.Fatal(err)
			}
			if err := s.flush(t.Context(), now, false); err != nil {
				t.Fatal(err)
			}
			if attempts != 4 || !reflect.DeepEqual(store.writes[0], store.writes[1]) || !reflect.DeepEqual(store.writes[0], store.writes[2]) || store.writes[3][0].Total != 30 {
				t.Fatalf("未知事务身份变化: %v", store.writes)
			}
			for _, step := range []int{60, 300, 3600} {
				var total float64
				var samples int64
				if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE step=?", step).Scan(&total, &samples); err != nil || total != 50 || samples != 2 {
					t.Fatalf("step=%d total=%v samples=%d err=%v", step, total, samples, err)
				}
			}
		})
	}
}

func TestHistoryDefiniteRollbackOnlyRetriesUncommittedData(t *testing.T) {
	real := openTestSQLiteStore(t)
	store := &batchRecordingStore{Store: real}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Minute)
	failure := errors.New("确定回滚")
	store.write = func(context.Context, []Bucket) (int, error) { return 3, failure }
	s.add(Sample{"node", at, map[string]float64{"cpu_percent": 20}})
	if err := s.flush(t.Context(), now, false); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	s.add(Sample{"node", at, map[string]float64{"cpu_percent": 30}})
	store.write = nil
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 2 || store.writes[1][0].Total != 50 || store.writes[1][0].Samples != 2 {
		t.Fatalf("重试批次=%v", store.writes)
	}
	for _, step := range []int{60, 300, 3600} {
		var total float64
		var samples int64
		if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE step=?", step).Scan(&total, &samples); err != nil || total != 50 || samples != 2 {
			t.Fatalf("step=%d total=%v samples=%d err=%v", step, total, samples, err)
		}
	}
}

func TestHistoryTypedRowAndTransactionBudgets(t *testing.T) {
	node := strings.Repeat("n", 64)
	metric := "inode_used_percent:" + strings.Repeat("x", 1024)
	key := bucketKey{node, math.MaxInt64}
	values := map[string]aggregate{metric: {math.MaxFloat64, math.MaxFloat64, math.MaxInt64}}
	if !validHistoryGroup(key, values) {
		t.Fatal("最长合法主键或有限数值被拒绝")
	}
	typed := func(kind, value string) [2]string { return [2]string{kind, value} }
	text := func(value string) [2]string { return typed("t", hex.EncodeToString([]byte(value))) }
	integer := typed("i", strconv.FormatInt(math.MinInt64, 10))
	floating := typed("f", strconv.FormatFloat(math.MaxFloat64, 'g', 26, 64))
	pk, err := json.Marshal([][2]string{text(node), text(metric), integer, integer})
	if err != nil {
		t.Fatal(err)
	}
	image, err := json.Marshal([][2]string{text(node), text(metric), integer, integer, floating, floating, integer})
	if err != nil {
		t.Fatal(err)
	}
	if len(pk)+len(image) > maxHistoryRowBytes || 3*maxFlushBuckets*maxHistoryRowBytes >= 8<<20 || historyTxHeaderBytes+3*maxFlushBuckets*maxHistoryRowBytes >= maxHistoryTxBytes {
		t.Fatalf("行日志=%d，批次预算超界", len(pk)+len(image))
	}
}

func TestHistoryFlushDrainsSamplesBetweenBatches(t *testing.T) {
	store := &batchRecordingStore{Store: &fakeStore{}}
	s := stoppedStoreService(t, store, nil, Options{})
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Minute)
	for i := range 256 {
		s.add(Sample{fmt.Sprintf("node-%03d", i), at, map[string]float64{"cpu_percent": 1}})
	}
	store.write = func(_ context.Context, buckets []Bucket) (int, error) {
		if len(store.writes) == 1 {
			s.samples <- Sample{"node-000", at, map[string]float64{"cpu_percent": 30}}
			s.samples <- Sample{"current", now, map[string]float64{"cpu_percent": 40}}
		}
		return 3 * len(buckets), nil
	}
	if err := s.flush(t.Context(), now, false); err != nil {
		t.Fatal(err)
	}
	if len(s.samples) != 0 || len(store.writes) != 2 || len(s.pending) != 2 || s.pending[bucketKey{"node-000", at.Unix()}]["cpu_percent"].total != 30 {
		t.Fatalf("迟到样本被旧批次消费: pending=%v", s.pending)
	}
	if err := s.flush(t.Context(), now, true); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 0 || len(store.writes) != 3 || len(store.writes[2]) != 2 || store.writes[2][0].Total != 30 || store.writes[2][1].Total != 40 {
		t.Fatalf("关闭刷写未排空迟到样本: %v", store.writes)
	}
}
