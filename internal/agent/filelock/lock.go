package filelock

import (
	"context"
	"time"
)

// Acquire locks an already-prepared, stable sidecar inode. Contention waits at
// most two seconds, or until the caller is canceled. No goroutine can acquire a
// lock after this function has returned. The uncontended path allocates no timer.
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
