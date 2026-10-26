//go:build linux

package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCABundleRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(secureCADirectory(t), "ca.fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := openCABundle(path, os.Geteuid())
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO was accepted as a CA bundle")
		}
	case <-time.After(500 * time.Millisecond):
		// Release a broken blocking opener so the regression never strands a
		// goroutine or waits indefinitely for an attacker-controlled writer.
		rescue, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("CA opener blocked on FIFO; unable to release it: %v", err)
		}
		defer rescue.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("CA opener remained blocked after FIFO writer appeared")
		}
		t.Fatal("CA validation waited for a FIFO writer instead of rejecting before opening")
	}
}
