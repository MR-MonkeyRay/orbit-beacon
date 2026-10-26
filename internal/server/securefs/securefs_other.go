//go:build !linux

package securefs

import (
	"errors"
	"os"
)

func validateOwner(_ string, _ os.FileInfo) error {
	return errors.New("secure local storage requires Linux ownership validation")
}

func openFile(_ string, _ int, _ os.FileMode) (*os.File, error) {
	return nil, errors.New("secure local storage requires Linux no-follow file access")
}
