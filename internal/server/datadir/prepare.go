package datadir

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"orbitbeacon/internal/server/securefs"
)

// Prepare validates all existing managed files before creating the private data
// directory or its persistent secrets key. Unsafe metadata is never repaired.
func Prepare(path string) error {
	dir, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := ValidateRestore(dir); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "secrets.key")
	keyExists, err := securefs.ValidateFile(keyPath)
	if err != nil {
		return err
	}
	if keyExists {
		return validateSecretsKey(keyPath)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if err := securefs.ValidateDirectory(dir); err != nil {
		return err
	}
	return ensureSecretsKey(keyPath)
}

// ValidateRestore checks only filesystem metadata, without creating files,
// changing permissions, or interpreting key or database contents. Missing
// directories and managed files are allowed for an empty restore target.
func ValidateRestore(path string) error {
	dir, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := securefs.ValidateParents(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		if err := securefs.ValidateDirectory(dir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, name := range []string{"secrets.key", "server.lock"} {
		if _, err := securefs.ValidateFile(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	for _, database := range []string{"orbitbeacon.db", "metrics.db"} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if _, err := securefs.ValidateFile(filepath.Join(dir, database+suffix)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSecretsKey(path string) error {
	key, err := LoadKey(path)
	clear(key)
	return err
}

// LoadKey reads an owner-only, non-symlink secrets key containing exactly 32
// bytes that are not all zero. The caller must clear the returned key when done.
func LoadKey(path string) ([]byte, error) {
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
