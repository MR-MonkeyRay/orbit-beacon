//go:build !linux

package main

import (
	"errors"
	"os"
)

func tryDataLock(*os.File) error {
	return errors.New("server data-directory locking requires Linux")
}
