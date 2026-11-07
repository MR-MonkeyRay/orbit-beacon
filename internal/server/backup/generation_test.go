package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestUploadReleasesWriteGateAndCancellationLeavesRunningTask(t *testing.T) {
	raw, opts := workerFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var service *Service
	uploader := &recordingUploader{}
	uploader.inspect = func(sendCtx context.Context, _, _ string) error {
		writeCtx, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		if _, err := service.CreateExport(writeCtx, restoreUser, []Category{Configuration}); err != nil {
			return err
		}
		cancel()
		<-sendCtx.Done()
		return sendCtx.Err()
	}
	opts.Uploader = uploader
	var err error
	service, err = New(mainHandle(t, raw), opts)
	if err != nil {
		t.Fatal(err)
	}
	id := "10000000-0000-4000-8000-000000000009"
	enqueueRemote(t, service, id, time.Now())
	lease, err := service.db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	task, err := service.claim(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	service.execute(lease.Context(), task)
	if !errors.Is(ctx.Err(), context.Canceled) || uploader.calls.Load() != 1 {
		t.Fatalf("upload did not reach cancellation: %v/%d", ctx.Err(), uploader.calls.Load())
	}
	var state, phase string
	if err := raw.QueryRow("SELECT state,phase FROM backup_tasks WHERE id=?", id).Scan(&state, &phase); err != nil {
		t.Fatal(err)
	}
	if state != "running" || phase != "storing" {
		t.Fatalf("canceled upload published a terminal result: %s/%s", state, phase)
	}
}
