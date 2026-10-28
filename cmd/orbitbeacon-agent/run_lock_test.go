//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/agent/filelock"
	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
)

func TestExitCodeIdentityConflict(t *testing.T) {
	for _, test := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{errors.New("failed"), 1},
		{&service.HTTPError{StatusCode: http.StatusConflict, Code: "other"}, 1},
		{&service.HTTPError{StatusCode: http.StatusBadRequest, Code: "identity_conflict"}, 1},
		{&service.HTTPError{StatusCode: http.StatusConflict, Code: "identity_conflict"}, exitIdentityConflict},
		{fmt.Errorf("report failed: %w", &service.HTTPError{StatusCode: http.StatusConflict, Code: "identity_conflict"}), exitIdentityConflict},
	} {
		if got := exitCode(test.err); got != test.want {
			t.Fatalf("exitCode(%v) = %d, want %d", test.err, got, test.want)
		}
	}
}

func TestRunLockAcrossProcesses(t *testing.T) {
	if directory := os.Getenv("ORBITBEACON_TEST_RUN_LOCK"); directory != "" {
		unlock, err := lockRun(context.Background(), directory)
		if unlock != nil {
			unlock()
		}
		if !errors.Is(err, filelock.ErrLocked) {
			t.Fatalf("second run did not reject the held lock: %v", err)
		}
		return
	}
	directory := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(context.Background())
	unlock, err := lockRun(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	state, err := identity.Open(context.Background(), filepath.Join(directory, "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := state.BeginRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Cancellation alone must not release ownership before runtime cleanup ends.
	cancel()
	command := exec.Command(os.Args[0], "-test.run=^TestRunLockAcrossProcesses$")
	command.Env = append(os.Environ(), "ORBITBEACON_TEST_RUN_LOCK="+directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("second process: %v\n%s", err, output)
	}
	after, err := state.Reload(context.Background())
	if err != nil || after != before {
		t.Fatalf("rejected run changed identity generation: %v", err)
	}
	unlock()
	unlock, err = lockRun(context.Background(), directory)
	if err != nil {
		t.Fatalf("run lock retained after shutdown: %v", err)
	}
	defer unlock()
}
