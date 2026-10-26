package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"orbitbeacon/internal/server/securefs"
)

var errDataDirInUse = errors.New("data_dir_in_use: another server command owns the data directory")

// Keep the lock inode across restarts; close releases ownership, never unlink.
func lockDataDir(path string) (*os.File, error) {
	if err := securefs.ValidateParents(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	if err := securefs.ValidateDirectory(path); err != nil {
		return nil, err
	}
	file, err := securefs.OpenFile(filepath.Join(path, "server.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := tryDataLock(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return file, nil
}
