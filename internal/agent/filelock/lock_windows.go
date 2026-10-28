//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func openLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}

func openPrivateLock(string) (*os.File, error) {
	return nil, errors.New("secure agent run locking is unsupported on Windows")
}

func tryLock(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func contended(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }

func release(file *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
	_ = file.Close()
}
