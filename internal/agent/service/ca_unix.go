//go:build unix

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maxCABundleSize = 1 << 20

// openCABundle pins every component before inspecting it. The caller must read
// this descriptor, never reopen the configured path after validation.
func openCABundle(path string, expectedOwner int) (*os.File, error) {
	invalid := errors.New("configured CA file must be an absolute administrator-owned regular file with safe non-writable ancestry")
	if !filepath.IsAbs(path) || expectedOwner < 0 {
		return nil, invalid
	}
	parts := strings.Split(path, "/")
	if parts[len(parts)-1] == "" {
		return nil, invalid
	}
	for _, part := range parts {
		if part == ".." {
			return nil, invalid
		}
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	dir, err := unix.Open("/", flags|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, invalid
	}
	defer func() { _ = unix.Close(dir) }()
	if !safeCADirectory(dir, expectedOwner) {
		return nil, invalid
	}
	for _, part := range parts[:len(parts)-1] {
		if part == "" {
			continue
		}
		next, err := unix.Openat(dir, part, flags|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, invalid
		}
		if !safeCADirectory(next, expectedOwner) {
			_ = unix.Close(next)
			return nil, invalid
		}
		_ = unix.Close(dir)
		dir = next
	}
	fd, err := unix.Openat(dir, parts[len(parts)-1], flags, 0)
	if err != nil {
		return nil, invalid
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07022 != 0 || uint64(stat.Uid) != uint64(expectedOwner) || stat.Size < 0 || stat.Size > maxCABundleSize {
		_ = unix.Close(fd)
		return nil, invalid
	}
	return os.NewFile(uintptr(fd), "configured CA bundle"), nil
}

func safeCADirectory(fd, expectedOwner int) bool {
	var stat unix.Stat_t
	return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Mode&0022 == 0 && (stat.Uid == 0 || uint64(stat.Uid) == uint64(expectedOwner))
}
