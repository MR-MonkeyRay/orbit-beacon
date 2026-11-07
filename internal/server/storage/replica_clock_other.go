//go:build !linux

package storage

import "errors"

func replicaClock() (string, int64, error) {
	return "", 0, errors.New("storage_replication_requires_linux")
}
