package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"orbitbeacon/internal/server/securefs"
)

// ErrInUse 表示另一个服务器命令正占用该数据目录。
var ErrInUse = errors.New("data_dir_in_use: another server command owns the data directory")

// Lock 持有数据目录的 server.lock，直到返回的文件被关闭。
// 锁 inode 跨重启保留；关闭时绝不 unlink。
func Lock(path string) (*os.File, error) {
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
