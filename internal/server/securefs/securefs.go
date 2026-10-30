package securefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ValidateParents rejects symlinks and ancestry writable by untrusted principals.
// Sticky system directories are allowed when their owner is trusted.
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

// ValidateDirectory accepts only an effective-user-owned, exactly 0700 directory.
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

// ValidateFile accepts absent files and owner-only regular files owned by the
// effective user. It never changes the file or follows its final symlink.
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

// OpenFile refuses a final symlink and validates the opened file as well as its
// directory entry, so callers never read or write a foreign or public file.
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

// OpenRegular opens an existing readable regular file without following symlinks.
// Unlike OpenFile, it accepts public or foreign-owned ciphertext inputs. Ancestry
// must still meet the shared pathname policy, and no write flags are accepted.
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

// SyncDirectory makes directory entry changes durable and closes the handle.
func SyncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
