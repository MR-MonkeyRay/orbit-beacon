//go:build linux

package securefs

import (
	"fmt"
	"os"
	"syscall"
)

func validateOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("filesystem component %q must be owned by the effective user", path)
	}
	return nil
}

func openFile(path string, flag int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
}
