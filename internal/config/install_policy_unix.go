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

// openInstallPolicy 从文件系统根开始，用固定的目录描述符逐级遍历。
// 全程不跟随符号链接，校验后也不按名称重新打开路径分量。
// 上级目录的属主可以是 root 或 expectedOwner。
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
		return nil, errors.New("策略路径必须指向普通文件")
	}
	// 不做路径规范化，避免掩盖 .. 前面的符号链接。
	for _, component := range components {
		if component == ".." {
			return nil, errors.New("策略路径不能包含父级回溯")
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
			return nil, fmt.Errorf("打开祖先目录 %q 失败: %w", component, err)
		}
		if err := validatePolicyDirectory(next, expectedOwner); err != nil {
			_ = unix.Close(next)
			return nil, fmt.Errorf("祖先目录 %q: %w", component, err)
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
		return nil, errors.New("策略必须是归指定管理员所有的普通文件，且组和其他用户均不可写")
	}
	if stat.Size < 0 || stat.Size > maxInstallPolicySize {
		_ = unix.Close(fd)
		return nil, errors.New("策略超过 64 KiB 大小限制")
	}
	return os.NewFile(uintptr(fd), path), nil
}

func validatePolicyDirectory(fd, expectedOwner int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0022 != 0 || (stat.Uid != 0 && uint64(stat.Uid) != uint64(expectedOwner)) {
		return errors.New("策略的上级目录必须归 root 或指定管理员所有，且组和其他用户均不可写")
	}
	return nil
}
