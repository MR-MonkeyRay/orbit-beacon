package history

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestHistoryShutdownResolvesUnknownFinalBatch(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			real := openTestSQLiteStore(t)
			store := &batchRecordingStore{Store: real}
			attempts, applied, resourceChecks := 0, 0, 0
			store.write = func(ctx context.Context, buckets []Bucket) (int, error) {
				attempts++
				if attempts == 1 {
					if committed {
						rows, err := real.Write(ctx, buckets)
						if err != nil {
							return rows, err
						}
						applied++
					}
					return 3 * len(buckets), storage.ErrCommitUnknown
				}
				if attempts == 2 {
					if !reflect.DeepEqual(store.writes[0], buckets) {
						return 0, errors.New("关闭解析改变了原批次")
					}
					// 下一次解析之前必须能够取得新租约和写 gate，不能仍占用原事务。
					checkCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
					lease, err := real.(*sqliteStore).db.Acquire(checkCtx, storage.Writer)
					if err == nil {
						var tx *storage.Tx
						tx, err = lease.BeginWrite(lease.Context())
						if err == nil {
							err = tx.Rollback()
						}
						lease.Close()
					}
					cancel()
					if err != nil {
						return 0, err
					}
					resourceChecks++
					if committed {
						return 3 * len(buckets), nil
					}
				}
				rows, err := real.Write(ctx, buckets)
				if err == nil {
					applied++
				}
				return rows, err
			}
			now := time.Now().UTC().Truncate(time.Minute)
			s := newService(store, nil, Options{Now: func() time.Time { return now }})
			defer s.Close()
			for i := range 256 {
				s.samples <- Sample{fmt.Sprintf("node-%03d", i), now, map[string]float64{"cpu_percent": 7}}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if attempts != 3 || applied != 2 || resourceChecks != 1 || len(s.pending) != 0 || len(s.inflight) != 0 {
				t.Fatalf("attempts=%d applied=%d resourceChecks=%d pending=%d inflight=%d", attempts, applied, resourceChecks, len(s.pending), len(s.inflight))
			}
			for _, step := range []int{60, 300, 3600} {
				var total float64
				var samples int64
				if err := observeStore(t, real.(*sqliteStore)).QueryRow("SELECT sum(total),sum(samples) FROM buckets WHERE step=?", step).Scan(&total, &samples); err != nil || total != 256*7 || samples != 256 {
					t.Fatalf("step=%d total=%v samples=%d err=%v", step, total, samples, err)
				}
			}
			if err := s.CloseContext(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoryShutdownUnknownFinalBatchHonorsDeadline(t *testing.T) {
	store := &batchRecordingStore{Store: &fakeStore{}}
	store.write = func(context.Context, []Bucket) (int, error) { return 3, storage.ErrCommitUnknown }
	now := time.Now().UTC().Truncate(time.Minute)
	s := newService(store, nil, Options{Now: func() time.Time { return now }})
	defer s.Close()
	s.samples <- Sample{"node", now, map[string]float64{"cpu_percent": 7}}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := s.Shutdown(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("关闭未遵守期限: elapsed=%v err=%v", time.Since(started), err)
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("关闭超时后刷写循环仍未退出")
	}
	if !errors.Is(s.err, storage.ErrCommitUnknown) || !errors.Is(s.err, context.DeadlineExceeded) || len(s.inflight) != 1 || len(store.writes) < 2 || len(store.writes) > 20 {
		t.Fatalf("关闭未保留未知结果或出现忙循环: attempts=%d inflight=%d err=%v", len(store.writes), len(s.inflight), s.err)
	}
	for _, batch := range store.writes {
		if !reflect.DeepEqual(batch, store.writes[0]) {
			t.Fatalf("期限内重试改变了原批次: %v", store.writes)
		}
	}
}

func TestHistoryShutdownDoesNotRetryOrdinaryWriteFailure(t *testing.T) {
	failure := errors.New("确定写入失败")
	store := &batchRecordingStore{Store: &fakeStore{}}
	store.write = func(context.Context, []Bucket) (int, error) { return 3, failure }
	now := time.Now().UTC().Truncate(time.Minute)
	s := newService(store, nil, Options{Now: func() time.Time { return now }})
	defer s.Close()
	s.samples <- Sample{"node", now, map[string]float64{"cpu_percent": 7}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, failure) || len(store.writes) != 1 {
		t.Fatalf("普通错误被盲目重试: attempts=%d err=%v", len(store.writes), err)
	}
}
