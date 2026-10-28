//go:build linux

package datadir

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryDataLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrInUse
	}
	return err
}
