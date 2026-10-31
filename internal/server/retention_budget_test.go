package server

import (
	"context"
	"testing"
	"time"
)

func TestRetentionBacklogBudgetCancelsSlowBatch(t *testing.T) {
	started := time.Now()
	calls := 0
	removed, err := cleanupRetentionBacklog(context.Background(), started, func(ctx context.Context, _ time.Time, _ int) (int64, error) {
		calls++
		if calls == 1 {
			return 1, nil
		}
		<-ctx.Done() // A stalled DELETE or busy wait.
		return 0, ctx.Err()
	})
	if err != nil || removed != 1 || calls != 2 {
		t.Fatalf("removed=%d calls=%d err=%v", removed, calls, err)
	}
	if elapsed := time.Since(started); elapsed > retentionCatchupBudget+time.Second {
		t.Fatalf("slow batch overran the budget: %v", elapsed)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cleanupRetentionBacklog(cancelled, started, func(ctx context.Context, _ time.Time, _ int) (int64, error) {
		return 0, ctx.Err()
	}); err == nil {
		t.Fatal("caller cancellation was reported as a budget stop")
	}
}

func TestRetentionBacklogUsesShortBatchesAndContinuesAfterDeferredFailure(t *testing.T) {
	remaining, calls := 1201, 0
	removed, err := cleanupRetentionBacklog(context.Background(), time.Now(), func(ctx context.Context, _ time.Time, limit int) (int64, error) {
		calls++
		if limit != 500 {
			t.Fatalf("batch limit=%d, want 500", limit)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("batch has no shared deadline")
		}
		// 备份清理将删除失败、需延后重试的文件计为已处理，并推迟下次尝试，
		// 让剩余积压仍能继续处理。
		n := min(remaining, limit)
		remaining -= n
		return int64(n), nil
	})
	if err != nil || removed != 1201 || remaining != 0 || calls != 4 {
		t.Fatalf("removed=%d remaining=%d calls=%d err=%v", removed, remaining, calls, err)
	}
}
