//go:build linux

package services

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestLinuxPingSocketsLoopback(t *testing.T) {
	for _, target := range []string{"127.0.0.1", "::1"} {
		t.Run(target, func(t *testing.T) {
			c := NewChecker()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			r := c.ping(ctx, target)
			if r.Error == "permission_denied" {
				t.Skip("kernel ping_group_range denies this user")
			}
			if !r.OK || r.LatencyMS == nil || r.PacketLoss == nil || *r.PacketLoss != 0 {
				t.Fatalf("loopback ping %+v", r)
			}
		})
	}
}

func TestPingSocketPermissionErrors(t *testing.T) {
	for _, e := range []error{syscall.EACCES, syscall.EPERM} {
		if !errors.Is(pingSocketError(e), errPermission) {
			t.Fatal(e)
		}
	}
	if errors.Is(pingSocketError(syscall.EAFNOSUPPORT), errPermission) {
		t.Fatal("unsupported family called permission denied")
	}
	if errorCode(syscall.EACCES) == "permission_denied" {
		t.Fatal("non-socket permission error classified as ping permission")
	}
}
