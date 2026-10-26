//go:build !unix

package service

import (
	"errors"
	"os"
)

const maxCABundleSize = 1 << 20

func openCABundle(string, int) (*os.File, error) {
	return nil, errors.New("secure configured CA loading is unsupported on this platform")
}
