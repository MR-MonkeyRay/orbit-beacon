package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/agent/filelock"
	v1 "orbitbeacon/internal/protocol/v1"
)

func configLockDesired(revision int64) v1.DesiredConfig {
	return v1.DesiredConfig{Revision: revision, ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, Interfaces: []string{"eth0"}, DiskPaths: []string{"/"}, ProbeTargets: []string{}, BillingDay: 1, QuotaBytes: "0"}
}

// Done is first observed when Acquire has encountered actual lock contention
// and constructs its bounded wait context. This avoids scheduler-dependent sleeps.
type configLockWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *configLockWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestConfigApplyLockCancellationKeepsCachedSnapshotsAvailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	prior := configLockDesired(1)
	store, err := OpenConfigStore(context.Background(), path, prior)
	if err != nil {
		t.Fatal(err)
	}
	release := holdConfigSidecar(t, path+".lock")
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &configLockWaitContext{Context: base, waiting: make(chan struct{})}
	applied := make(chan error, 1)
	go func() { applied <- store.Apply(ctx, configLockDesired(2)) }()
	select {
	case <-ctx.waiting:
	case err := <-applied:
		t.Fatalf("Apply returned before waiting for the held sidecar: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Apply never reached lock contention")
	}

	type snapshot struct {
		current  v1.DesiredConfig
		revision int64
		interval int
		failure  *v1.ConfigError
	}
	read := make(chan snapshot, 1)
	go func() {
		current := store.Current()
		revision, interval := store.settings()
		read <- snapshot{current: current, revision: revision, interval: interval, failure: store.ConfigError()}
	}()
	select {
	case got := <-read:
		if !reflect.DeepEqual(got.current, prior) || got.revision != prior.Revision || got.interval != prior.ReportIntervalSeconds || got.failure != nil {
			t.Fatalf("cached snapshot changed while waiting: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("cached config reads blocked behind the external lock")
	}

	cancel()
	select {
	case err := <-applied:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Apply cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Apply did not return promptly after cancellation")
	}
	persisted, err := loadConfig(path)
	if err != nil || !reflect.DeepEqual(persisted, prior) || !reflect.DeepEqual(store.Current(), prior) {
		t.Fatalf("canceled Apply changed durable or cached config: %+v, %v", persisted, err)
	}
	if failure := store.ConfigError(); failure == nil || failure.Revision != 2 || failure.Code != "persist_failed" {
		t.Fatalf("lock failure has wrong ConfigError semantics: %+v", failure)
	}

	release()
	next := configLockDesired(2)
	if err := store.Apply(context.Background(), next); err != nil {
		t.Fatalf("Apply after sidecar release: %v", err)
	}
	persisted, err = loadConfig(path)
	if err != nil || !reflect.DeepEqual(persisted, next) || !reflect.DeepEqual(store.Current(), next) || store.ConfigError() != nil {
		t.Fatalf("later Apply did not publish durable config and clear failure: %+v, %v", persisted, err)
	}
}

func TestOpenConfigStoreRespectsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if store, err := OpenConfigStore(ctx, path, configLockDesired(1)); !errors.Is(err, context.Canceled) || store != nil {
		t.Fatalf("canceled OpenConfigStore = %v, %v", store, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled open wrote desired config: %v", err)
	}
}

func TestConfigOpenLockCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	holdConfigSidecar(t, path+".lock")
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &configLockWaitContext{Context: base, waiting: make(chan struct{})}
	opened := make(chan error, 1)
	go func() {
		_, err := OpenConfigStore(ctx, path, configLockDesired(1))
		opened <- err
	}()
	select {
	case <-ctx.waiting:
	case err := <-opened:
		t.Fatalf("OpenConfigStore returned before sidecar contention: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OpenConfigStore never reached lock contention")
	}
	cancel()
	select {
	case err := <-opened:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OpenConfigStore cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenConfigStore did not return promptly after cancellation")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled open changed desired config: %v", err)
	}
}

func holdConfigSidecar(t *testing.T, path string) func() {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestConfigSidecarLockHolder$")
	command.Env = append(os.Environ(), "ORBITBEACON_CONFIG_LOCK_PATH="+path)
	command.Stderr = os.Stderr
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = input.Close()
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("sidecar holder failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-exited
			t.Error("sidecar holder did not exit")
		}
	}
	t.Cleanup(release)
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(output).ReadString('\n')
		if err == nil && line != "locked\n" {
			err = fmt.Errorf("unexpected lock holder output %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("sidecar holder readiness: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sidecar holder never acquired the lock")
	}
	return release
}

func TestConfigSidecarLockHolder(t *testing.T) {
	path := os.Getenv("ORBITBEACON_CONFIG_LOCK_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	unlock, err := filelock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}
