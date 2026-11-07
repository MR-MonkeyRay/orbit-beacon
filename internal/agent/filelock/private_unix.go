//go:build !windows

package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openPrivateLock(path string) (*os.File, error) {
	fail := errors.New("run lock requires a regular 0600 file in a private state directory owned by the effective UID, without symlinks")
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return nil, fail
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fail
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	dir, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, fail
	}
	// 逐级固定目录，防止被替换的上级目录重定向锁文件的打开。
	for _, part := range strings.Split(filepath.Dir(absolute), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(dir, part, flags, 0)
		_ = unix.Close(dir)
		if err != nil {
			return nil, fail
		}
		dir = next
	}
	defer unix.Close(dir)
	var directory unix.Stat_t
	if err := unix.Fstat(dir, &directory); err != nil || directory.Uid != uint32(os.Geteuid()) || directory.Mode&0077 != 0 {
		return nil, fail
	}
	fd, err := unix.Openat(dir, filepath.Base(absolute), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fail
	}
	file := os.NewFile(uintptr(fd), "agent run lock")
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 || info.Uid != uint32(os.Geteuid()) || info.Nlink != 1 {
		_ = file.Close()
		return nil, fail
	}
	return file, nil
}
