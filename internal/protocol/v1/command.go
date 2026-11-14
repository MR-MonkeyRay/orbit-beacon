package v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Command struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Command) UnmarshalJSON(data []byte) error {
	type wire Command
	var decoded wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*c = Command(decoded)
	return nil
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
	case RemoteCommandCheckUpdate, RemoteCommandUpgrade, RemoteCommandRestart, RemoteCommandStop:
	default:
		return errors.New("unknown command kind")
	}
	return nil
}

type CommandResult struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Code   string      `json:"code"`
	Update *UpdateInfo `json:"update,omitempty"`
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
	if r.Update != nil {
		return r.Update.Validate()
	}
	return nil
}

type UpdateInfo struct {
	Channel        string    `json:"channel"`
	CurrentVersion string    `json:"current_version"`
	LatestVersion  string    `json:"latest_version"`
	CheckedAt      time.Time `json:"checked_at"`
}

func (u UpdateInfo) Validate() error {
	if u.Channel != "github" && u.Channel != "custom" {
		return errors.New("invalid update channel")
	}
	if _, err := ParseVersion(u.CurrentVersion); err != nil {
		return err
	}
	if _, err := ParseVersion(u.LatestVersion); err != nil {
		return err
	}
	if u.CheckedAt.IsZero() || u.CheckedAt.Year() < 1 || u.CheckedAt.Year() > 9999 {
		return errors.New("invalid update check time")
	}
	return nil
}
