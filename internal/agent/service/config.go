package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"

	"orbitbeacon/internal/agent/filelock"
	v1 "orbitbeacon/internal/protocol/v1"
)

type ConfigStore struct {
	mu            sync.RWMutex
	path          string
	current       v1.DesiredConfig
	failure       *v1.ConfigError
	syncDirectory func(string) error
}

func OpenConfigStore(ctx context.Context, path string, initial v1.DesiredConfig) (*ConfigStore, error) {
	return openConfigStore(ctx, path, initial, syncConfigDirectory)
}

func openConfigStore(ctx context.Context, path string, initial v1.DesiredConfig, syncDirectory func(string) error) (*ConfigStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	unlock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	store := &ConfigStore{path: path, syncDirectory: syncDirectory}
	current, err := loadConfig(path)
	existing := !errors.Is(err, os.ErrNotExist)
	var failure *v1.ConfigError
	if errors.Is(err, os.ErrNotExist) {
		if err := initial.Validate(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := store.saveConfig(initial); err != nil {
			return nil, err
		}
		current = initial
	} else if errors.Is(err, v1.ErrReportBudget) {
		// Preserve the old file and revision, but keep the repair channel alive.
		// The error prevents the server from acknowledging these diagnostics as
		// successful application of the oversized desired configuration.
		current.DiskPaths = []string{}
		current.Interfaces = []string{}
		if err := current.Validate(); err != nil {
			return nil, err
		}
		failure = &v1.ConfigError{Revision: current.Revision, Code: "report_size_exceeded"}
	} else if err != nil {
		return nil, err
	}
	if existing {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := store.syncDirectory(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	store.current = cloneConfig(current)
	store.failure = failure
	return store, nil
}

func loadConfig(path string) (v1.DesiredConfig, error) {
	var current v1.DesiredConfig
	info, err := os.Lstat(path)
	if err != nil {
		return current, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		return current, errors.New("desired config must be a regular 0600 file")
	}
	file, err := os.Open(path)
	if err != nil {
		return current, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&current); err != nil {
		return current, errors.New("invalid saved desired config")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return current, errors.New("invalid desired config trailing data")
	}
	if err := current.Validate(); err != nil {
		return current, err
	}
	return current, nil
}

func (s *ConfigStore) Current() v1.DesiredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.current)
}

func (s *ConfigStore) settings() (revision int64, reportInterval int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current.Revision, s.current.ReportIntervalSeconds
}

func (s *ConfigStore) ConfigError() *v1.ConfigError {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failure == nil {
		return nil
	}
	failure := *s.failure
	if failure.Fields != nil {
		failure.Fields = make(map[string]string, len(s.failure.Fields))
		for key, value := range s.failure.Fields {
			failure.Fields[key] = value
		}
	}
	return &failure
}

func (s *ConfigStore) Apply(ctx context.Context, next v1.DesiredConfig) error {
	fail := func(err error) error {
		s.failure = &v1.ConfigError{Revision: max(0, next.Revision), Code: "invalid_config"}
		var validation *v1.ValidationError
		if errors.As(err, &validation) {
			s.failure.Fields = validation.Fields
		}
		return err
	}
	if err := next.Validate(); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return fail(err)
	}
	unlock, err := filelock.Acquire(ctx, s.path+".lock")
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return fail(err)
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	current, err := loadConfig(s.path)
	matchesSaved := err == nil
	if errors.Is(err, os.ErrNotExist) {
		current = s.current
	} else if err != nil && !errors.Is(err, v1.ErrReportBudget) {
		return fail(err)
	}
	if s.current.Revision > current.Revision {
		current = s.current
		matchesSaved = false
	}
	if next.Revision < current.Revision {
		return fail(errors.New("desired config revision decreased"))
	}
	if next.Revision == current.Revision {
		if !reflect.DeepEqual(next, current) {
			return fail(errors.New("conflicting desired config revision"))
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if !matchesSaved {
			if err := s.saveConfig(next); err != nil {
				return fail(err)
			}
		} else if err := s.syncDirectory(filepath.Dir(s.path)); err != nil {
			return fail(err)
		}
		s.current = cloneConfig(current)
		s.failure = nil
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := s.saveConfig(next); err != nil {
		return fail(err)
	}
	s.current = cloneConfig(next)
	s.failure = nil
	return nil
}

func cloneConfig(value v1.DesiredConfig) v1.DesiredConfig {
	if value.Interfaces != nil {
		value.Interfaces = append([]string{}, value.Interfaces...)
	}
	if value.DiskPaths != nil {
		value.DiskPaths = append([]string{}, value.DiskPaths...)
	}
	if value.ProbeTargets != nil {
		value.ProbeTargets = append([]string{}, value.ProbeTargets...)
	}
	return value
}

func (s *ConfigStore) saveConfig(value v1.DesiredConfig) error {
	path := s.path
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("desired config destination is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".desired-*")
	if err != nil {
		return err
	}
	defer file.Close()
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return s.syncDirectory(filepath.Dir(path))
}

func syncConfigDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
