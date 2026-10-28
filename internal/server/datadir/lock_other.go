//go:build !linux

package datadir

import (
	"errors"
	"os"
)

func tryDataLock(*os.File) error {
	return errors.New("server data-directory locking requires Linux")
}
