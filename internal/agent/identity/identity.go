package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"orbitbeacon/internal/agent/filelock"
)

type Identity struct {
	InstallationID   string `json:"installation_id"`
	NodeToken        string `json:"node_token"`
	PendingNodeToken string `json:"pending_node_token,omitempty"`
	NodeID           string `json:"node_id,omitempty"`
	RunGeneration    uint64 `json:"run_generation"`
	SessionID        string `json:"session_id,omitempty"`
}

type Options struct{ Random io.Reader }

type Store struct {
	mu            sync.Mutex
	path          string
	random        io.Reader
	current       Identity
	syncDirectory func(string) error
}

// Open coordinates inspection and first creation with atomic identity updates.
// Inspection never begins a run.
func Open(ctx context.Context, path string, options Options) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := prepareNewDirectory(directory); err != nil {
		return nil, err
	}
	unlock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	store := &Store{path: path, random: random, syncDirectory: syncIdentityDirectory}
	current, err := load(path)
	if err == nil {
		if err := store.syncDirectory(filepath.Dir(path)); err != nil {
			return nil, err
		}
		store.current = current.Identity
		return store, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	installation, err := randomID(random, 16)
	if err != nil {
		return nil, err
	}
	token, err := randomID(random, 32)
	if err != nil {
		return nil, err
	}
	current = diskIdentity{Identity: Identity{InstallationID: installation, NodeToken: token}}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.persist(current, true); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		current, err = load(path)
		if err != nil {
			return nil, err
		}
		if err := store.syncDirectory(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	store.current = current.Identity
	return store, nil
}

func (s *Store) Current() Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// Reload refreshes the in-memory identity under the same cross-process lock used
// for durable updates. It never creates or changes credentials.
func (s *Store) Reload(ctx context.Context) (Identity, error) {
	unlock, err := filelock.Acquire(ctx, s.path+".lock")
	if err != nil {
		return Identity{}, err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	current, err := load(s.path)
	if err != nil {
		return Identity{}, err
	}
	if err := s.syncDirectory(filepath.Dir(s.path)); err != nil {
		return Identity{}, err
	}
	s.current = current.Identity
	return current.Identity, nil
}

// BeginRun publishes the next generation only after the replacement is durable.
func (s *Store) BeginRun(ctx context.Context) (Identity, error) {
	unlock, err := filelock.Acquire(ctx, s.path+".lock")
	if err != nil {
		return Identity{}, err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	current, err := load(s.path)
	if err != nil {
		return Identity{}, err
	}
	if current.RunGeneration == math.MaxUint64 {
		return Identity{}, errors.New("identity run generation exhausted")
	}
	session, err := randomID(s.random, 16)
	if err != nil {
		return Identity{}, err
	}
	current.RunGeneration++
	current.SessionID = session
	if err := s.persist(current, false); err != nil {
		return Identity{}, err
	}
	s.current = current.Identity
	return current.Identity, nil
}

// ConfirmEnrollment promotes only the exact durable credential acknowledged by the server.
func (s *Store) ConfirmEnrollment(ctx context.Context, token, nodeID string) error {
	unlock, err := filelock.Acquire(ctx, s.path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := load(s.path)
	if err != nil {
		return err
	}
	if nodeID == "" || (current.NodeID != "" && current.NodeID != nodeID) || (token != current.NodeToken && token != current.PendingNodeToken) {
		return errors.New("enrollment confirmation does not match durable identity")
	}
	if token == current.NodeToken && current.NodeID == nodeID {
		if err := s.syncDirectory(filepath.Dir(s.path)); err != nil {
			return err
		}
		s.current = current.Identity
		return nil
	}
	if token == current.PendingNodeToken {
		current.NodeToken = token
		current.PendingNodeToken = ""
	}
	current.NodeID = nodeID
	pruneEnrollmentAttempts(&current)
	if err := s.persist(current, false); err != nil {
		return err
	}
	s.current = current.Identity
	return nil
}

func randomID(random io.Reader, size int) (string, error) {
	data := make([]byte, size)
	if _, err := io.ReadFull(random, data); err != nil {
		return "", errors.New("identity entropy unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func prepareNewDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("identity directory must be private and not a symlink")
	}
	if runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0 {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("identity directory must be private before use")
	}
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	return checkDirectory(path)
}

func checkDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("identity directory must be private and not a symlink")
	}
	return nil
}

func load(path string) (diskIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return diskIdentity{}, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		return diskIdentity{}, errors.New("identity must be a regular 0600 file, not a symlink")
	}
	if err := checkDirectory(filepath.Dir(path)); err != nil {
		return diskIdentity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return diskIdentity{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return diskIdentity{}, errors.New("identity changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	var value diskIdentity
	if err := decoder.Decode(&value); err != nil {
		return diskIdentity{}, errors.New("invalid identity file")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return diskIdentity{}, errors.New("invalid identity trailing data")
	}
	token, err := base64.RawURLEncoding.DecodeString(value.NodeToken)
	if err != nil || len(token) != 32 || value.InstallationID == "" || (value.RunGeneration > 0 && value.SessionID == "") {
		return diskIdentity{}, errors.New("invalid identity fields")
	}
	if value.PendingNodeToken != "" {
		pending, err := base64.RawURLEncoding.DecodeString(value.PendingNodeToken)
		if err != nil || len(pending) != 32 || value.PendingNodeToken == value.NodeToken {
			return diskIdentity{}, errors.New("invalid pending identity credential")
		}
	}
	if err := validateEnrollmentAttempts(value); err != nil {
		return diskIdentity{}, err
	}
	return value, nil
}

func (s *Store) persist(value diskIdentity, create bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 64*1024 {
		return errors.New("identity state exceeds 64 KiB")
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".identity-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
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
	if create {
		// Link is an atomic no-replace publication, unlike Rename on POSIX.
		if err := os.Link(name, s.path); err != nil {
			return err
		}
		if err := os.Remove(name); err != nil {
			return err
		}
	} else if err := os.Rename(name, s.path); err != nil {
		return err
	}
	return s.syncDirectory(filepath.Dir(s.path))
}

func syncIdentityDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync identity directory: %w", err)
	}
	return nil
}
