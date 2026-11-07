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
		// 实现有误而阻塞时，解除打开 FIFO 的阻塞，避免测试遗留 goroutine，
		// 或无限等待攻击者控制的写入方。
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
