//go:build !unix

package config

import (
	"errors"
	"os"
)

func openInstallPolicy(path string, expectedOwner int) (*os.File, error) {
	return nil, errors.New("secure administrator-owned install policy loading is unsupported on this platform")
}
