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
