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
	fail := errors.New("enrollment token must be a regular 0600 file owned by the effective UID in a directory owned by root or the effective UID, without symlinks")
	absolute, err := filepath.Abs(path)
	if err != nil || strings.HasSuffix(path, "/") {
		return nil, errors.New("invalid enrollment token path")
	}
	// Reject traversal before cleaning can hide a symlink component.
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return nil, fail
		}
	}
	parts := strings.Split(absolute, "/")
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	dir, err := openTicketDirectory(filepath.Dir(absolute))
	if err != nil {
		return nil, errors.Join(fail, errors.New("cannot open enrollment token directory"), err)
	}
	var parent unix.Stat_t
	if err := unix.Fstat(dir, &parent); err != nil {
		_ = unix.Close(dir)
		return nil, errors.Join(fail, err)
	}
	// The immediate directory may be group/other writable; the file itself must
	// have the exact owner and mode checked below, and cleanup checks its inode.
	if parent.Uid != 0 && parent.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(dir)
		return nil, errors.Join(fail, fmt.Errorf("enrollment token directory must be owned by root or the effective UID (uid=%d)", parent.Uid))
	}
	name := parts[len(parts)-1]
	fd, err := unix.Openat(dir, name, flags, 0)
	if err != nil {
		_ = unix.Close(dir)
		return nil, errors.Join(fail, errors.New("cannot open enrollment token file"), err)
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
		return nil, errors.Join(fail, fmt.Errorf("enrollment token file has unsafe type, owner or mode (uid=%d mode=%s)", stat.Uid, info.Mode()))
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		ticket.close()
		return nil, errors.New("invalid enrollment token file size")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		ticket.close()
		return nil, errors.New("invalid enrollment token file")
	}
	ticket.info, ticket.token = info, token
	return ticket, nil
}

// openTicketDirectory keeps the entire walk descriptor-relative so no ancestor
// can redirect ticket reads or cleanup through a symlink.
func openTicketDirectory(absolute string) (int, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_DIRECTORY
	dir, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(absolute, "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(dir, part, flags, 0)
		_ = unix.Close(dir)
		if err != nil {
			return -1, err
		}
		dir = next
	}
	return dir, nil
}

func (t *enrollmentTicket) close() {
	_ = t.file.Close()
	_ = unix.Close(t.directory)
}

func (t *enrollmentTicket) remove() error {
	// Detect a renamed/replaced parent before consuming any directory entry.
	// The subsequent operation still uses the pinned descriptor, not this path.
	parent, err := openTicketDirectory(t.parentPath)
	if err != nil {
		return err
	}
	var current unix.Stat_t
	err = unix.Fstat(parent, &current)
	_ = unix.Close(parent)
	if err != nil || current.Dev != t.parentInfo.Dev || current.Ino != t.parentInfo.Ino || current.Uid != t.parentInfo.Uid {
		return errors.New("enrollment ticket directory changed")
	}
	var entry, original unix.Stat_t
	if err := unix.Fstatat(t.directory, t.name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstat(int(t.file.Fd()), &original); err != nil {
		return err
	}
	if entry.Dev != original.Dev || entry.Ino != original.Ino || entry.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("enrollment ticket path changed")
	}
	// Rename into a new private directory before comparing inodes. A separate
	// stat/unlink pair on the original name would permit replacement between
	// those operations and could unlink an unrelated file.
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
		// Never overwrite a concurrent replacement, even when restoring a raced
		// entry. If restoration is impossible, retain it in the private directory.
		if restoreErr := unix.Renameat2(dir, "ticket", t.directory, t.name, unix.RENAME_NOREPLACE); restoreErr == nil {
			_ = unix.Unlinkat(t.directory, name, unix.AT_REMOVEDIR)
		}
		_ = unix.Fsync(t.directory)
		return errors.New("enrollment ticket path changed")
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
