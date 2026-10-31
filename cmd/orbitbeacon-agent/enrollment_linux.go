//go:build linux

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type enrollmentTicket struct {
	directory  int
	name       string
	parentPath string
	parentInfo unix.Stat_t
	file       *os.File
	info       os.FileInfo
	token      string
}

func openEnrollmentTicket(path string) (*enrollmentTicket, error) {
	fail := errors.New("注册票据必须是权限为 0600、属主为当前有效 UID 的普通文件；上级目录须归 root 或当前有效 UID 所有，路径不能含符号链接")
	absolute, err := filepath.Abs(path)
	if err != nil || strings.HasSuffix(path, "/") {
		return nil, errors.New("注册票据路径无效")
	}
	// 先拒绝 .. 路径分量，避免路径规范化掩盖符号链接。
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return nil, fail
		}
	}
	parts := strings.Split(absolute, "/")
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	dir, err := openTicketDirectory(filepath.Dir(absolute))
	if err != nil {
		return nil, errors.Join(fail, errors.New("无法打开注册票据目录"), err)
	}
	var parent unix.Stat_t
	if err := unix.Fstat(dir, &parent); err != nil {
		_ = unix.Close(dir)
		return nil, errors.Join(fail, err)
	}
	if err := validateTicketDirectory(&parent); err != nil {
		_ = unix.Close(dir)
		return nil, errors.Join(fail, err)
	}
	name := parts[len(parts)-1]
	fd, err := unix.Openat(dir, name, flags, 0)
	if err != nil {
		_ = unix.Close(dir)
		return nil, errors.Join(fail, errors.New("无法打开注册票据文件"), err)
	}
	file := os.NewFile(uintptr(fd), "enrollment ticket")
	ticket := &enrollmentTicket{directory: dir, name: name, parentPath: filepath.Dir(absolute), parentInfo: parent, file: file}
	info, err := file.Stat()
	var stat unix.Stat_t
	if err != nil {
		ticket.close()
		return nil, errors.Join(fail, err)
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		ticket.close()
		return nil, errors.Join(fail, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || stat.Uid != uint32(os.Geteuid()) {
		ticket.close()
		return nil, errors.Join(fail, fmt.Errorf("注册票据文件的类型、属主或权限不安全（uid=%d mode=%s）", stat.Uid, info.Mode()))
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		ticket.close()
		return nil, errors.New("注册票据文件大小无效")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		ticket.close()
		return nil, errors.New("注册票据文件无效")
	}
	ticket.info, ticket.token = info, token
	return ticket, nil
}

// openTicketDirectory 让整个遍历都基于描述符相对路径，任何祖先目录都无法通过
// 符号链接重定向票据的读取或清理。
func openTicketDirectory(absolute string) (int, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_DIRECTORY
	dir, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, err
	}
	if err := validateTicketDirectoryFD(dir); err != nil {
		_ = unix.Close(dir)
		return -1, err
	}
	for _, part := range strings.Split(absolute, "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(dir, part, flags, 0)
		if err != nil {
			_ = unix.Close(dir)
			return -1, err
		}
		if err := validateTicketDirectoryFD(next); err != nil {
			_ = unix.Close(next)
			_ = unix.Close(dir)
			return -1, err
		}
		_ = unix.Close(dir)
		dir = next
	}
	return dir, nil
}

func validateTicketDirectoryFD(fd int) error {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	return validateTicketDirectory(&info)
}

func validateTicketDirectory(info *unix.Stat_t) error {
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("注册票据路径组件不是目录")
	}
	euid := uint32(os.Geteuid())
	if info.Uid != 0 && info.Uid != euid {
		return fmt.Errorf("注册票据目录必须由 root 或有效 UID 所有（uid=%d）", info.Uid)
	}
	const writableByGroupOrOther = 0022
	const sticky = 01000
	if info.Mode&writableByGroupOrOther != 0 && info.Mode&sticky == 0 {
		return fmt.Errorf("注册票据目录可被组或其他用户写入（mode=%#o）", info.Mode&07777)
	}
	return nil
}

func (t *enrollmentTicket) close() {
	_ = t.file.Close()
	_ = unix.Close(t.directory)
}

func (t *enrollmentTicket) remove() error {
	// 删除目录项前先检测父目录是否被改名或替换。
	// 后续操作仍使用已固定的描述符，而不是此路径。
	parent, err := openTicketDirectory(t.parentPath)
	if err != nil {
		return err
	}
	var current unix.Stat_t
	err = unix.Fstat(parent, &current)
	_ = unix.Close(parent)
	if err != nil || current.Dev != t.parentInfo.Dev || current.Ino != t.parentInfo.Ino || current.Uid != t.parentInfo.Uid {
		return errors.New("注册票据目录已变化")
	}
	var entry, original unix.Stat_t
	if err := unix.Fstatat(t.directory, t.name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstat(int(t.file.Fd()), &original); err != nil {
		return err
	}
	if entry.Dev != original.Dev || entry.Ino != original.Ino || entry.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("注册票据路径已变化")
	}
	// 先改名到新的私有目录，再比较 inode。若对原名单独做 stat/unlink，
	// 两步之间文件可能被替换，还可能误删无关文件。
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".orbitbeacon-ticket-" + hex.EncodeToString(random[:])
	if err := unix.Mkdirat(t.directory, name, 0700); err != nil {
		return err
	}
	dir, err := unix.Openat(t.directory, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err := unix.Renameat2(t.directory, t.name, dir, "ticket", unix.RENAME_NOREPLACE); err != nil {
		_ = unix.Unlinkat(t.directory, name, unix.AT_REMOVEDIR)
		return err
	}
	fd, err := unix.Openat(dir, "ticket", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	matches := false
	if err == nil {
		file := os.NewFile(uintptr(fd), "enrollment ticket cleanup")
		info, statErr := file.Stat()
		matches = statErr == nil && os.SameFile(t.info, info)
		_ = file.Close()
	}
	if !matches {
		// 即使恢复竞争中产生的目录项，也绝不覆盖并发写入的替换文件。
		// 无法恢复时，把票据留在私有目录中。
		if restoreErr := unix.Renameat2(dir, "ticket", t.directory, t.name, unix.RENAME_NOREPLACE); restoreErr == nil {
			_ = unix.Unlinkat(t.directory, name, unix.AT_REMOVEDIR)
		}
		_ = unix.Fsync(t.directory)
		return errors.New("注册票据路径已变化")
	}
	if err := unix.Unlinkat(dir, "ticket", 0); err != nil {
		if restoreErr := unix.Renameat2(dir, "ticket", t.directory, t.name, unix.RENAME_NOREPLACE); restoreErr == nil {
			_ = unix.Unlinkat(t.directory, name, unix.AT_REMOVEDIR)
		}
		_ = unix.Fsync(t.directory)
		return err
	}
	if err := unix.Fsync(dir); err != nil {
		return err
	}
	if err := unix.Unlinkat(t.directory, name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return unix.Fsync(t.directory)
}
