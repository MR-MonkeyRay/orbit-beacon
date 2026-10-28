package collect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestBlockedDiskCannotBlockCancellationOrSpawnMoreReaders(t *testing.T) {
	path := t.TempDir()
	config := v1.RuntimeSettings{ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, DiskPaths: []string{path}, Interfaces: []string{"lo"}}
	c, err := New(Options{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	c.diskUsage = func(context.Context, string) (*disk.UsageStat, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release // Like statfs, this deliberately ignores context cancellation.
		return &disk.UsageStat{Total: 100, Used: 20}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		metrics v1.Metrics
		err     error
	}
	done := make(chan result, 1)
	go func() {
		metrics, err := c.Collect(ctx)
		done <- result{metrics, err}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("disk read never started")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("collection error=%v", got.err)
		}
		if err := got.metrics.Validate(); err != nil {
			t.Fatalf("cancelled disk destroyed partial snapshot: %v", err)
		}
		if len(got.metrics.Disks) != 1 || got.metrics.Disks[0].Path != path || got.metrics.Disks[0].TotalBytes != nil {
			t.Fatalf("blocked disk produced fabricated data: %+v", got.metrics.Disks)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("uncancellable disk read blocked collection cancellation")
	}
	for range 3 {
		ctx, stop := context.WithTimeout(context.Background(), time.Second)
		metrics, err := c.Collect(ctx)
		stop()
		if err != nil {
			t.Fatalf("pending disk prevented subsequent snapshots: %v", err)
		}
		if err := metrics.Validate(); err != nil || len(metrics.Networks) != 1 {
			t.Fatalf("pending disk blocked other configured sources: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("blocked disk leaked readers across samples: %d", got)
	}
}

func TestLateDiskReadCannotRestoreRemovedSelection(t *testing.T) {
	oldPath, newPath := t.TempDir(), t.TempDir()
	config := v1.RuntimeSettings{ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, DiskPaths: []string{oldPath}, Interfaces: []string{"lo"}}
	c, err := New(Options{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	c.diskUsage = func(_ context.Context, path string) (*disk.UsageStat, error) {
		if path == oldPath {
			close(entered)
			<-release
			return &disk.UsageStat{Total: 999, Used: 999}, nil
		}
		return &disk.UsageStat{Total: 100, Used: 20}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = c.Collect(ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old disk read never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old disk prevented cancellation")
	}
	config.DiskPaths = []string{newPath}
	if err := c.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	waiting, err := c.Collect(context.Background())
	if err != nil || len(waiting.Disks) != 1 || waiting.Disks[0].Path != newPath || waiting.Disks[0].TotalBytes != nil {
		t.Fatalf("pending old read contaminated new selection: %+v %v", waiting.Disks, err)
	}
	pending := c.diskPending
	close(release)
	released = true
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("released disk did not finish")
	}
	current, err := c.Collect(context.Background())
	if err != nil || len(current.Disks) != 1 || current.Disks[0].Path != newPath || current.Disks[0].TotalBytes == nil || *current.Disks[0].TotalBytes != 100 {
		t.Fatalf("late disk result restored old selection: %+v %v", current.Disks, err)
	}
	if err := current.Validate(); err != nil {
		t.Fatalf("new disk result invalid: %v", err)
	}
}
