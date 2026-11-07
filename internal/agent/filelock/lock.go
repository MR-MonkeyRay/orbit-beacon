package filelock

import (
	"context"
	"errors"
	"time"
)

var ErrLocked = errors.New("another agent run is already using this state directory")

// TryAcquire 不等待锁，释放时也不删除独立锁文件，以保持 inode 不变。
// 删除它会让并发进程在同一路径上锁定不同的 inode。
func TryAcquire(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openPrivateLock(path)
	if err != nil {
		return nil, err
	}
	if err := tryLock(file); err != nil {
		_ = file.Close()
		if contended(err) {
			return nil, ErrLocked
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		release(file)
		return nil, err
	}
	return func() { release(file) }, nil
}

// Acquire 锁定已准备好的独立锁文件，其 inode 必须保持不变。
// 竞争时最多等待两秒，也可由调用方提前取消；返回后不会有 goroutine 继续获取锁。
// 无竞争时不分配定时器。
func Acquire(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openLock(path)
	if err != nil {
		return nil, err
	}
	err = tryLock(file)
	if err == nil {
		if err := ctx.Err(); err != nil {
			release(file)
			return nil, err
		}
		return func() { release(file) }, nil
	}
	if !contended(err) {
		_ = file.Close()
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-wait.Done():
			_ = file.Close()
			return nil, wait.Err()
		case <-ticker.C:
			if err := wait.Err(); err != nil {
				_ = file.Close()
				return nil, err
			}
			err = tryLock(file)
			if err == nil {
				if err := wait.Err(); err != nil {
					release(file)
					return nil, err
				}
				return func() { release(file) }, nil
			}
			if !contended(err) {
				_ = file.Close()
				return nil, err
			}
		}
	}
}
