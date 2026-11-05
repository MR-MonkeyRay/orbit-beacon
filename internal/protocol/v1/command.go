package v1

import (
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
)

type Command struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	TargetVersion string    `json:"target_version,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

var commandVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,63}$`)

func (c Command) Validate() error {
	if _, err := uuid.Parse(c.ID); err != nil || len(c.ID) != 36 {
		return errors.New("invalid command ID")
	}
	if c.ExpiresAt.IsZero() || c.ExpiresAt.Year() < 1 || c.ExpiresAt.Year() > 9999 {
		return errors.New("invalid command expiry")
	}
	switch c.Kind {
	case RemoteCommandUpgrade:
		if !commandVersion.MatchString(c.TargetVersion) {
			return errors.New("invalid target version")
		}
	case RemoteCommandRestart, RemoteCommandStop:
		if c.TargetVersion != "" {
			return errors.New("unexpected target version")
		}
	default:
		return errors.New("unknown command kind")
	}
	return nil
}

type CommandResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Code   string `json:"code"`
}

func (r CommandResult) Validate() error {
	if _, err := uuid.Parse(r.ID); err != nil || len(r.ID) != 36 {
		return errors.New("invalid command result ID")
	}
	switch r.Status {
	case "succeeded", "failed", "rejected":
	default:
		return errors.New("unknown command result status")
	}
	switch r.Code {
	case "ok", "disabled_locally", "unsupported", "expired", "interrupted", "failed":
	default:
		return errors.New("unknown command result code")
	}
	return nil
}
