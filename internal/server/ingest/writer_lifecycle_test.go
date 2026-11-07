package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWriterRejectsCanceledContextBeforeAdmission(t *testing.T) {
	service := NewService(nil, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.beginUse(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled writer admission returned %v", err)
	}
}

func TestWriterQueueIsBoundedAndShutdownWaitsForCommittedWork(t *testing.T) {
	service := NewService(nil, Options{})
	finish, err := service.beginUse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstRelease, err := service.queueWrite(context.Background(), finish)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	results := make(chan error, writerQueueCapacity-1)
	for range writerQueueCapacity - 1 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			finish, err := service.beginUse(context.Background())
			if err != nil {
				results <- err
				return
			}
			release, err := service.queueWrite(context.Background(), finish)
			results <- err
			if err == nil {
				release()
			}
		}()
	}
	deadline := time.Now().Add(time.Second)
	for len(service.writerQueue) != writerQueueCapacity && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(service.writerQueue) != writerQueueCapacity {
		t.Fatalf("writer queue was not filled to its bound: got %d want %d", len(service.writerQueue), writerQueueCapacity)
	}
	finish, err = service.beginUse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.queueWrite(context.Background(), finish); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("overflow was not rejected as writer busy: %v", err)
	}
	firstRelease()
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.beginUse(context.Background()); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("closed writer accepted new work: %v", err)
	}
}
