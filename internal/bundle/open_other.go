//go:build !linux

package bundle

import (
	"errors"
	"os"
)

var errSecureOpenUnsupported = errors.New("bundle verification requires Linux no-follow descriptor access")

func openBundleDirectory(string) (*os.File, error)           { return nil, errSecureOpenUnsupported }
func openBundleFile(*os.File, string) (*os.File, error)      { return nil, errSecureOpenUnsupported }
func openRegularPath(string) (*os.File, error)               { return nil, errSecureOpenUnsupported }
func readBundleFile(*os.File, string, int64) ([]byte, error) { return nil, errSecureOpenUnsupported }
