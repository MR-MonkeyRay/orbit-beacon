package ingest

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestAcceptCanceledWhileWaitingForDatabaseWriterDoesNotPublish(t *testing.T) {
	f := newCoalesceFixture(t, time.Second)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	lease, err := f.nodes.Database().Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	f.now = f.now.Add(time.Minute)
	f.report.Seq++
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.service.accept(requestCtx, f.id, nil, 0, f.report)
		done <- err
	}()
	for len(f.service.writerQueue) == 0 {
		select {
		case err := <-done:
			t.Fatalf("acceptance did not wait for writer: %v", err)
		case <-ctx.Done():
			t.Fatal("acceptance did not reach writer admission")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acceptance = %v", err)
	}
	latest, err := f.service.Latest(ctx, f.id)
	if err != nil || latest.Seq == f.report.Seq {
		t.Fatalf("canceled acceptance published: seq=%d err=%v", latest.Seq, err)
	}
}
