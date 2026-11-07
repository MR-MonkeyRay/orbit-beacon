package history

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFlushMetricsWithCanceledWrite(t *testing.T) {
	s, _ := testService(t, nil)
	stopped(t, s)
	calls, entries := 0, 0
	s.observeFlush = func(elapsed time.Duration, n int) {
		calls++
		entries = n
		if elapsed < 0 {
			t.Error("negative flush duration")
		}
	}
	if _, _, ok := s.DatabaseStats(); !ok {
		t.Fatal("SQLite history hid database stats")
	}
	now := time.Now().UTC().Truncate(time.Minute)
	if err := s.flush(context.Background(), now, false); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("empty flush observed")
	}
	s.pending[bucketKey{"old", now.Add(-time.Minute).Unix()}] = map[string]aggregate{"cpu": {samples: 1}, "memory": {samples: 1}}
	s.pending[bucketKey{"current", now.Unix()}] = map[string]aggregate{"cpu": {samples: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.flush(ctx, now, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush error=%v", err)
	}
	if calls != 1 || entries != 6 {
		t.Fatalf("observations=%d entries=%d", calls, entries)
	}
	if err := s.flush(ctx, now, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown flush error=%v", err)
	}
	if calls != 2 || entries != 9 || len(s.pending) != 2 {
		t.Fatalf("observations=%d entries=%d pending=%d", calls, entries, len(s.pending))
	}
	// 关闭采集时不会调用观测器，原有错误行为保持一致。
	s.observeFlush = nil
	if err := s.flush(ctx, now, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("disabled flush error=%v", err)
	}
	if calls != 2 {
		t.Fatal("disabled observer called")
	}
}
