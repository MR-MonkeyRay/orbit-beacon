package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

// ExitError lets the caller unwind all deferred cleanup before main exits.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	if e.Code == 75 {
		return "remote restart requested"
	}
	return "remote stop requested"
}

const commandHistoryLimit = 32
const pendingCommandResultLimit = 3 * commandHistoryLimit

type processedCommand struct {
	ID            string    `json:"id"`
	TargetVersion string    `json:"target_version,omitempty"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	Code          string    `json:"code"`
	ExpiresAt     time.Time `json:"expires_at"`
}
type commandState struct {
	Processed []processedCommand `json:"processed"`
	Pending   []v1.CommandResult `json:"pending"`
}

// Only the reporting goroutine owns this store. Corruption or any ambiguous
// durability failure disables execution until an operator repairs local state.
type commandStore struct {
	version string
	path    string
	state   commandState
	blocked bool
	logger  *slog.Logger
	persist func(commandState) error
}

func openCommandStore(path string, logger *slog.Logger, version string) *commandStore {
	s := &commandStore{path: path, logger: logger, version: version}
	s.persist = s.write
	if err := s.load(); err != nil {
		s.block(err)
	}
	return s
}
func (s *commandStore) block(err error) {
	s.blocked = true
	s.logger.Error("command_state_unavailable", "error", err)
}
func (s *commandStore) load() error {
	info, err := os.Lstat(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("command directory must be private and not a symlink")
	}
	info, err = os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s.save(commandState{Processed: []processedCommand{}, Pending: []v1.CommandResult{}})
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("commands must be a regular 0600 file")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return errors.New("command state changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	var state commandState
	if err := decoder.Decode(&state); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid command trailing data")
	}
	if len(state.Processed) > commandHistoryLimit || len(state.Pending) > pendingCommandResultLimit {
		return errors.New("command state exceeds bounds")
	}
	seen := map[string]processedCommand{}
	for i := range state.Processed {
		c := &state.Processed[i]
		c.ID = strings.ToLower(c.ID)
		command := v1.Command{ID: c.ID, Kind: c.Kind, TargetVersion: c.TargetVersion, ExpiresAt: c.ExpiresAt}
		if err := command.Validate(); err != nil {
			return err
		}
		if _, exists := seen[c.ID]; exists {
			return errors.New("duplicate command state")
		}
		if c.State == "executing" {
			if c.Kind != v1.RemoteCommandRestart && c.Kind != v1.RemoteCommandStop && c.Kind != v1.RemoteCommandUpgrade {
				return errors.New("invalid executing command")
			}
		} else if err := (v1.CommandResult{ID: c.ID, Status: c.State, Code: c.Code}).Validate(); err != nil {
			return err
		}
		seen[c.ID] = *c
	}
	pending := map[string]bool{}
	for i := range state.Pending {
		r := &state.Pending[i]
		r.ID = strings.ToLower(r.ID)
		if err := r.Validate(); err != nil {
			return err
		}
		c, exists := seen[r.ID]
		if pending[r.ID] || (exists && (c.State != r.Status || c.Code != r.Code)) || (!exists && (r.Status != "rejected" || r.Code != "failed")) {
			return errors.New("inconsistent command result state")
		}
		pending[r.ID] = true
	}
	for i := range state.Processed {
		c := &state.Processed[i]
		if c.State != "executing" {
			continue
		}
		// Starting proves nothing: a rollback must not inherit a recorded success.
		if c.Kind == v1.RemoteCommandUpgrade && s.version == c.TargetVersion && s.upgradeUnconfirmed() {
			continue
		}
		c.State, c.Code = "failed", "interrupted"
		if c.Kind == v1.RemoteCommandRestart {
			c.State, c.Code = "succeeded", "ok"
		}
		if c.Kind == v1.RemoteCommandUpgrade {
			if s.version == c.TargetVersion {
				c.State, c.Code = "succeeded", "ok"
			} else if _, err := os.Stat(filepath.Join(filepath.Dir(s.path), "upgrade-rolled-back")); err == nil {
				c.Code = "rolled_back"
			}
		}
		state.Pending = append(state.Pending, v1.CommandResult{ID: c.ID, Status: c.State, Code: c.Code})
	}
	if len(state.Pending) > pendingCommandResultLimit {
		return errors.New("command recovery results exceed bounds")
	}
	if err := s.save(state); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(filepath.Dir(s.path), "upgrade-rolled-back")); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("upgrade_rollback_marker_remove_failed", "error", err)
	}
	return nil
}
func (s *commandStore) save(state commandState) error {
	if err := s.persist(state); err != nil {
		return err
	}
	s.state = state
	return nil
}
func (s *commandStore) copyState() commandState {
	return commandState{Processed: append([]processedCommand{}, s.state.Processed...), Pending: append([]v1.CommandResult{}, s.state.Pending...)}
}
func (s *commandStore) write(state commandState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".commands-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *commandStore) results() []v1.CommandResult {
	return append([]v1.CommandResult{}, s.state.Pending[:min(2, len(s.state.Pending))]...)
}
func (s *commandStore) acknowledge(results []v1.CommandResult) {
	if s.blocked || len(results) == 0 {
		return
	}
	state := s.copyState()
	for _, sent := range results {
		for i, r := range state.Pending {
			if r == sent {
				state.Pending = append(state.Pending[:i], state.Pending[i+1:]...)
				break
			}
		}
	}
	if err := s.save(state); err != nil {
		s.block(err)
	}
}

// handle returns an action only after its durable execution barrier has synced.
func (s *commandStore) handle(c v1.Command, serverTime time.Time, settings v1.RuntimeSettings) string {
	if err := c.Validate(); err != nil {
		s.logger.Warn("invalid_remote_command")
		return ""
	}
	if s.blocked {
		return ""
	}
	c.ID = strings.ToLower(c.ID)
	for _, r := range s.state.Pending {
		if r.ID == c.ID {
			return ""
		}
	}
	for _, previous := range s.state.Processed {
		if previous.ID == c.ID {
			return ""
		}
	}
	if serverTime.IsZero() {
		s.logger.Warn("command_server_time_missing")
		return ""
	}
	state := s.copyState()
	// Retain unexpired IDs even under command floods. A fixed 32-entry FIFO alone
	// permits replay after eviction; only acknowledged AND expired IDs are safe.
	if len(state.Processed) == commandHistoryLimit {
		evict := -1
		for i, p := range state.Processed {
			if p.State == "executing" || p.ExpiresAt.After(serverTime) {
				continue
			}
			pending := false
			for _, r := range state.Pending {
				if r.ID == p.ID {
					pending = true
					break
				}
			}
			if !pending {
				evict = i
				break
			}
		}
		if evict < 0 {
			s.logger.Warn("command_history_full")
			// Capacity rejection has no side effect. Keep it in the durable result
			// queue until a 200 response; accepted execution IDs remain untouched.
			if len(state.Pending) >= pendingCommandResultLimit-commandHistoryLimit {
				return ""
			}
			state.Pending = append(state.Pending, v1.CommandResult{ID: c.ID, Status: "rejected", Code: "failed"})
			if err := s.save(state); err != nil {
				s.block(err)
			}
			return ""
		}
		state.Processed = append(state.Processed[:evict], state.Processed[evict+1:]...)
	}
	status, code := "executing", ""
	switch {
	case !serverTime.Before(c.ExpiresAt) || c.Kind == v1.RemoteCommandUpgrade && c.ExpiresAt.Sub(serverTime) < upgradeReserve:
		status, code = "rejected", "expired"
	case !settings.AllowsCommand(c.Kind):
		status, code = "rejected", "disabled_locally"
	}
	state.Processed = append(state.Processed, processedCommand{ID: c.ID, Kind: c.Kind, TargetVersion: c.TargetVersion, State: status, Code: code, ExpiresAt: c.ExpiresAt})
	if status != "executing" {
		state.Pending = append(state.Pending, v1.CommandResult{ID: c.ID, Status: status, Code: code})
	}
	if err := s.save(state); err != nil {
		s.block(err)
		return ""
	}
	if status != "executing" {
		return ""
	}
	if c.Kind == v1.RemoteCommandStop {
		state = s.copyState()
		last := &state.Processed[len(state.Processed)-1]
		last.State, last.Code = "succeeded", "ok"
		state.Pending = append(state.Pending, v1.CommandResult{ID: c.ID, Status: "succeeded", Code: "ok"})
		if err := s.save(state); err != nil {
			s.block(err)
			return ""
		}
	}
	return c.Kind
}

func (s *commandStore) finishUpgrade(result upgradeOutcome) {
	if s.blocked {
		return
	}
	state := s.copyState()
	for i := range state.Processed {
		c := &state.Processed[i]
		if c.ID == result.id && c.State == "executing" {
			c.State, c.Code = result.status, result.code
			state.Pending = append(state.Pending, v1.CommandResult{ID: c.ID, Status: c.State, Code: c.Code})
			if err := s.save(state); err != nil {
				s.block(err)
			}
			return
		}
	}
}
func (s *commandStore) upgradeUnconfirmed() bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(s.path), "upgrade-pending"))
	return err == nil
}

// confirmUpgrade runs on the first accepted report. Rollback is disarmed durably
// before success is recorded, so a crash between the steps cannot roll back onto
// a saved success: load() derives it from the version once the marker is gone.
func (s *commandStore) confirmUpgrade() bool {
	dir := filepath.Dir(s.path)
	err := os.Remove(filepath.Join(dir, "upgrade-pending"))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	} else if err == nil {
		err = syncDirectory(dir)
	}
	if err != nil {
		s.logger.Warn("upgrade_pending_remove_failed", "error", err)
		return false
	}
	if s.blocked {
		return true
	}
	state := s.copyState()
	changed := false
	for i := range state.Processed {
		c := &state.Processed[i]
		if c.Kind == v1.RemoteCommandUpgrade && c.State == "executing" && c.TargetVersion == s.version {
			c.State, c.Code = "succeeded", "ok"
			state.Pending = append(state.Pending, v1.CommandResult{ID: c.ID, Status: c.State, Code: c.Code})
			changed = true
		}
	}
	if changed {
		if err := s.save(state); err != nil {
			s.block(err)
		}
	}
	return true
}
