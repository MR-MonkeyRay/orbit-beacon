//go:build linux

package storage

import (
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"sync"
)

var replicaBoot struct {
	sync.Once
	id  string
	err error
}

func replicaClock() (string, int64, error) {
	replicaBoot.Do(func() {
		var b []byte
		b, replicaBoot.err = os.ReadFile("/proc/sys/kernel/random/boot_id")
		replicaBoot.id = strings.TrimSpace(string(b))
	})
	if replicaBoot.err != nil {
		return "", 0, replicaBoot.err
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return "", 0, err
	}
	return replicaBoot.id, ts.Nano(), nil
}
