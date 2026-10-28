package inbox

import (
	"encoding/json"
	"strconv"
	"time"
)

// ValidateDetails checks the immutable snapshot shapes used by notification
// consumers without looking up live sources or performing any side effects.
// Fields are optional for historical snapshots; unknown metadata is preserved.
func ValidateDetails(kind string, data json.RawMessage) error {
	if len(data) > 64<<10 {
		return ErrInvalidInput
	}
	fields, ok := detailsObject(data)
	if !ok {
		return ErrInvalidInput
	}
	switch kind {
	case "task":
		if !detailsStrings(fields, false, "id", "kind", "state", "phase", "error_code", "warning_code") ||
			!detailsStrings(fields, true, "plan_id", "actor_id", "artifact_name", "artifact_bytes", "artifact_sha256", "result_location") ||
			!detailsTimes(fields, false, "created_at") ||
			!detailsTimes(fields, true, "scheduled_at", "started_at", "finished_at", "artifact_expires_at", "password_expires_at") {
			return ErrInvalidInput
		}
		if raw, present := fields["categories"]; present {
			var categories []string
			if json.Unmarshal(raw, &categories) != nil || categories == nil {
				return ErrInvalidInput
			}
			// These are the archive protocol's three independent categories. The
			// inbox must not import backup (which inserts task notifications).
			for _, category := range categories {
				if category != "configuration" && category != "monitoring" && category != "events" {
					return ErrInvalidInput
				}
			}
		}
		if raw, present := fields["destination"]; present && !detailsDestination(raw) {
			return ErrInvalidInput
		}
		if raw, present := fields["artifact_bytes"]; present && string(raw) != "null" {
			var decimal string
			if json.Unmarshal(raw, &decimal) != nil {
				return ErrInvalidInput
			}
			value, err := strconv.ParseUint(decimal, 10, 64)
			if err != nil || strconv.FormatUint(value, 10) != decimal {
				return ErrInvalidInput
			}
		}
		for _, name := range []string{"restored", "download_available", "password_available", "has_password"} {
			if raw, present := fields[name]; present {
				var value bool
				if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
					return ErrInvalidInput
				}
			}
		}
	case "alert":
		// IDs are historical display references, not live foreign keys. Empty
		// IDs and unknown string metric/severity metadata retain UI fallbacks.
		if !detailsStrings(fields, false, "rule_id", "rule_name", "node_id", "node_name", "metric", "severity", "transition") ||
			!detailsTimes(fields, false, "started_at") || !detailsTimes(fields, true, "resolved_at") {
			return ErrInvalidInput
		}
		if raw, present := fields["value"]; present {
			var value *float64
			// JSON decoding also rejects non-finite/overflowing float values.
			if json.Unmarshal(raw, &value) != nil {
				return ErrInvalidInput
			}
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

func detailsObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

func detailsStrings(fields map[string]json.RawMessage, nullable bool, names ...string) bool {
	for _, name := range names {
		if raw, present := fields[name]; present {
			if string(raw) == "null" {
				if !nullable {
					return false
				}
				continue
			}
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return false
			}
		}
	}
	return true
}

func detailsTimes(fields map[string]json.RawMessage, nullable bool, names ...string) bool {
	if !detailsStrings(fields, nullable, names...) {
		return false
	}
	for _, name := range names {
		if raw, present := fields[name]; present && string(raw) != "null" {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return false
			}
			// Producers marshal time.Time, not fixed-width database timekeys.
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return false
			}
		}
	}
	return true
}

func detailsDestination(raw json.RawMessage) bool {
	fields, ok := detailsObject(raw)
	if !ok || !detailsStrings(fields, false, "plan_name", "actor_name") {
		return false
	}
	if target, present := fields["target"]; present && string(target) != "null" {
		fields, ok = detailsObject(target)
		if !ok || !detailsStrings(fields, false, "path", "provider", "endpoint", "region", "bucket", "prefix") {
			return false
		}
		for _, name := range []string{"path_style", "allow_private_networks"} {
			if raw, present := fields[name]; present {
				var value bool
				if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
					return false
				}
			}
		}
		if raw, present := fields["retain_count"]; present {
			var count int
			if string(raw) == "null" || json.Unmarshal(raw, &count) != nil {
				return false
			}
		}
	}
	return true
}
