package securefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ValidateParents 拒绝符号链接及可被不可信主体写入的祖先目录。
// 属主可信时允许带 sticky 位的系统目录。
func ValidateParents(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && !info.IsDir() {
			return fmt.Errorf("parent %q must be a directory, not a symlink or other file", parent)
		}
		if err == nil {
			if err := validateAncestor(parent, info); err != nil {
				return err
			}
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
	}
}

// ValidateDirectory 仅接受归进程有效用户所有、权限恰为 0700 的目录。
func ValidateDirectory(path string) error {
	if err := ValidateParents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("directory %q must have mode 0700 and not be a symlink", path)
	}
	return validateOwner(path, info)
}

// ValidateFile 接受不存在的文件，或归进程有效用户所有且禁止组与其他用户访问的普通文件。
// 不修改文件本身，也不跟随其末级符号链接。
func ValidateFile(path string) (bool, error) {
	if err := ValidateParents(path); err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false, fmt.Errorf("file %q must be an owner-only regular file", path)
	}
	if err := validateOwner(path, info); err != nil {
		return false, err
	}
	return true, nil
}

// OpenFile 拒绝末级符号链接，并校验打开的文件及其目录项，
// 确保调用方不会读写他人文件或公开文件。
func OpenFile(path string, flag int, mode os.FileMode) (*os.File, error) {
	if err := ValidateParents(path); err != nil {
		return nil, err
	}
	file, err := openFile(path, flag, mode)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0) {
		err = fmt.Errorf("file %q must be an owner-only regular file", path)
	}
	if err == nil {
		err = validateOwner(path, info)
	}
	if err == nil {
		entry, statErr := os.Lstat(path)
		if statErr != nil {
			err = statErr
		} else if !os.SameFile(info, entry) {
			err = fmt.Errorf("file %q changed while being opened", path)
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// OpenRegular 打开已有的可读普通文件，不跟随符号链接。
// 与 OpenFile 不同，它接受公开或他人拥有的密文输入。
// 祖先路径仍须满足共享路径名策略，且不接受写标志。
func OpenRegular(path string) (*os.File, error) {
	if err := ValidateParents(path); err != nil {
		return nil, err
	}
	file, err := openFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("file %q must be a regular file", path)
	}
	if err == nil {
		entry, statErr := os.Lstat(path)
		if statErr != nil {
			err = statErr
		} else if !os.SameFile(info, entry) || !entry.Mode().IsRegular() {
			err = fmt.Errorf("file %q changed while being opened", path)
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// SyncDirectory 使目录项变更持久化并关闭句柄。
func SyncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
