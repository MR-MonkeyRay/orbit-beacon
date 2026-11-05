package history

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestFlushMetricsWithoutDatabaseIO(t *testing.T) {
	// 不打开连接；已取消的上下文确保本测试不会执行 SQL 或文件操作。
	db, err := sql.Open("sqlite", "unused-history-metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reader, err := sql.Open("sqlite", "unused-history-reader")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	db.SetMaxOpenConns(3)
	reader.SetMaxOpenConns(4)
	calls, entries := 0, 0
	s := &Service{db: db, reader: reader, pending: make(map[bucketKey]map[string]aggregate), observeFlush: func(elapsed time.Duration, n int) {
		calls++
		entries = n
		if elapsed < 0 {
			t.Error("negative flush duration")
		}
	}}
	writerStats, readerStats := s.DatabaseStats()
	if writerStats.MaxOpenConnections != 3 || readerStats.MaxOpenConnections != 4 {
		t.Fatalf("pool stats=%+v %+v", writerStats, readerStats)
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
