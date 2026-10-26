//go:build unix

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openInstallPolicy walks from the filesystem root using pinned directory
// descriptors. No component is followed through a symlink or reopened by name
// after validation. Shared ancestors may be owned by root or expectedOwner.
func openInstallPolicy(path string, expectedOwner int) (*os.File, error) {
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		path = cwd + "/" + path
	}
	components := strings.Split(path, "/")
	if components[len(components)-1] == "" {
		return nil, errors.New("policy path must name a regular file")
	}
	// Do not clean the path: cleaning could hide a symlink before a '..'.
	for _, component := range components {
		if component == ".." {
			return nil, errors.New("policy path must not contain parent traversal")
		}
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	directory, err := unix.Open("/", flags|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(directory) }()
	if err := validatePolicyDirectory(directory, expectedOwner); err != nil {
		return nil, err
	}
	for _, component := range components[:len(components)-1] {
		if component == "" {
			continue
		}
		next, err := unix.Openat(directory, component, flags|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, fmt.Errorf("open ancestor %q: %w", component, err)
		}
		if err := validatePolicyDirectory(next, expectedOwner); err != nil {
			_ = unix.Close(next)
			return nil, fmt.Errorf("ancestor %q: %w", component, err)
		}
		_ = unix.Close(directory)
		directory = next
	}
	fd, err := unix.Openat(directory, components[len(components)-1], flags, 0)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0022 != 0 || uint64(stat.Uid) != uint64(expectedOwner) {
		_ = unix.Close(fd)
		return nil, errors.New("policy must be a regular file owned by the expected administrator and not writable by group or others")
	}
	if stat.Size < 0 || stat.Size > maxInstallPolicySize {
		_ = unix.Close(fd)
		return nil, errors.New("policy exceeds the 64 KiB size limit")
	}
	return os.NewFile(uintptr(fd), path), nil
}

func validatePolicyDirectory(fd, expectedOwner int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0022 != 0 || (stat.Uid != 0 && uint64(stat.Uid) != uint64(expectedOwner)) {
		return errors.New("policy ancestors must be directories owned by root or the expected administrator and not writable by group or others")
	}
	return nil
}
