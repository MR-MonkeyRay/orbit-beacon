package v1

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Command struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	TargetVersion string    `json:"target_version,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

var commandVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Version 按十进制字符串比较，避免数值溢出。
type Version [3]string

func ParseVersion(raw string) (Version, error) {
	if len(raw) > 64 || !commandVersion.MatchString(raw) {
		return Version{}, errors.New("invalid target version")
	}
	parts := strings.Split(raw, ".")
	return Version{parts[0], parts[1], parts[2]}, nil
}

func (v Version) Compare(other Version) int {
	for i := range v {
		if len(v[i]) < len(other[i]) {
			return -1
		}
		if len(v[i]) > len(other[i]) {
			return 1
		}
		if c := strings.Compare(v[i], other[i]); c != 0 {
			return c
		}
	}
	return 0
}

func (c Command) Validate() error {
	if _, err := uuid.Parse(c.ID); err != nil || len(c.ID) != 36 {
		return errors.New("invalid command ID")
	}
	if c.ExpiresAt.IsZero() || c.ExpiresAt.Year() < 1 || c.ExpiresAt.Year() > 9999 {
		return errors.New("invalid command expiry")
	}
	switch c.Kind {
	case RemoteCommandUpgrade:
		if _, err := ParseVersion(c.TargetVersion); err != nil {
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
	case "ok", "disabled_locally", "unsupported", "expired", "interrupted", "failed", "download", "checksum", "self_check", "downgrade", "rolled_back":
	default:
		return errors.New("unknown command result code")
	}
	return nil
}
