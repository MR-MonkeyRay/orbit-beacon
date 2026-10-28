package backup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type schedulerBlockingUploader struct {
	entered chan string
	release chan struct{}
}

func (u *schedulerBlockingUploader) Upload(ctx context.Context, _ ObjectDestination, _ ObjectCredentials, _ string, key string, _ string) (UploadResult, error) {
	select {
	case u.entered <- key:
	case <-ctx.Done():
		return UploadResult{}, ctx.Err()
	}
	select {
	case <-u.release:
		return UploadResult{}, nil
	case <-ctx.Done():
		return UploadResult{}, ctx.Err()
	}
}
func TestSchedulerRunTicksWhileWorkerBusyAndTerminalWake(t *testing.T) {
	db, opts := workerFixture(t)
	var clock atomic.Int64
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock.Store(start.UnixNano())
	opts.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	opts.Location = time.UTC
	uploader := &schedulerBlockingUploader{entered: make(chan string, 4), release: make(chan struct{}, 4)}
	opts.Uploader = uploader
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not await worker cancellation")
		}
	}()
	patch := localPlanPatch(t)
	patch.Kind = new("object")
	patch.Destination = []byte(`{"provider":"s3_compatible","endpoint":"https://objects.example.test","region":"test","bucket":"backups"}`)
	patch.Credentials = &ObjectCredentials{AccessKeyID: "test", SecretAccessKey: "secret"}
	patch.Enabled = new(true)
	patch.Cron = new("* * * * *")
	p, err := s.SavePlan(ctx, "", patch)
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(time.Minute).UnixNano())
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-uploader.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("startup Tick did not execute due plan")
	}
	clock.Store(start.Add(11 * time.Minute).UnixNano())
	other := localPlanPatch(t)
	other.Enabled = new(true)
	other.Cron = new("* * * * *")
	q, err := s.SavePlan(ctx, "", other)
	if err != nil {
		t.Fatal(err)
	}
	// A mutation wakes the sole scheduler even though the upload worker is occupied.
	clock.Store(start.Add(12 * time.Minute).UnixNano())
	s.signal()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		if err = db.QueryRow(`SELECT count(*) FROM backup_tasks WHERE plan_id=? AND state='queued'`, q.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduler stalled behind worker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	wait, err := s.schedulerWait(ctx, opts.Now())
	if err != nil || wait <= 0 || wait > time.Minute {
		t.Fatalf("overdue active timer spins: %v %v", wait, err)
	}
	uploader.release <- struct{}{}
	// Terminal completion must coalesce ten elapsed periods immediately, not wait
	// for the minute fallback. The unrelated local task remains FIFO work.
	deadline = time.Now().Add(5 * time.Second)
	for {
		var count int
		if err = db.QueryRow(`SELECT count(*) FROM backup_tasks WHERE plan_id=?`, p.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal wake did not enqueue coalesced cycle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run cancellation blocked")
	}
	if err = s.Tick(context.Background(), opts.Now()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
