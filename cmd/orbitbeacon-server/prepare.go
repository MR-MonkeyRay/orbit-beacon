package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"orbitbeacon/internal/server/securefs"
)

// prepareDataDir rejects unsafe existing components rather than repairing their
// permissions. All existing files are checked before anything is created.
func prepareDataDir(path string) error {
	return prepareDataDirWithDatabaseValidation(path, true)
}

// prepareRestoreDataDir validates the directory and secrets key but leaves
// database files for restore to validate, preserve, or replace atomically.
func prepareRestoreDataDir(path string) error {
	return prepareDataDirWithDatabaseValidation(path, false)
}

func prepareDataDirWithDatabaseValidation(path string, validateDatabaseFiles bool) error {
	dir, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := securefs.ValidateParents(dir); err != nil {
		return err
	}
	_, err = os.Lstat(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	exists := err == nil
	if exists {
		if err := securefs.ValidateDirectory(dir); err != nil {
			return err
		}
	}
	keyPath := filepath.Join(dir, "secrets.key")
	keyExists, err := securefs.ValidateFile(keyPath)
	if err != nil {
		return err
	}
	if keyExists {
		if err := validateSecretsKey(keyPath); err != nil {
			return err
		}
	}
	if validateDatabaseFiles {
		for _, name := range []string{"orbitbeacon.db", "orbitbeacon.db-wal", "orbitbeacon.db-shm"} {
			if _, err := securefs.ValidateFile(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	if !exists {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create data directory: %w", err)
		}
		if err := securefs.ValidateDirectory(dir); err != nil {
			return err
		}
	}
	if keyExists {
		return nil
	}
	return ensureSecretsKey(keyPath)
}

func validateSecretsKey(path string) error {
	key, err := loadSecretsKey(path)
	clear(key)
	return err
}
func loadSecretsKey(path string) ([]byte, error) {
	file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != 32 {
		return nil, errors.New("secrets.key must contain exactly 32 bytes")
	}
	var key [32]byte
	defer clear(key[:])
	if _, err := io.ReadFull(file, key[:]); err != nil {
		return nil, fmt.Errorf("read secrets.key: %w", err)
	}
	if key == [32]byte{} {
		return nil, errors.New("secrets.key must not be all zero")
	}
	return append([]byte(nil), key[:]...), nil
}

func ensureSecretsKey(path string) error {
	exists, err := securefs.ValidateFile(path)
	if err != nil {
		return err
	}
	if exists {
		return validateSecretsKey(path)
	}
	var key [32]byte
	defer clear(key[:])
	if _, err := rand.Read(key[:]); err != nil {
		return fmt.Errorf("generate secrets.key: %w", err)
	}
	if key == [32]byte{} {
		return errors.New("generated secrets.key is all zero")
	}
	file, err := securefs.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return validateSecretsKey(path)
	}
	if err != nil {
		return fmt.Errorf("create secrets.key: %w", err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if n, err := file.Write(key[:]); err != nil {
		return fmt.Errorf("write secrets.key: %w", err)
	} else if n != len(key) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync secrets.key: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close secrets.key: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync data directory: %w", err)
	}
	complete = true
	return nil
}
