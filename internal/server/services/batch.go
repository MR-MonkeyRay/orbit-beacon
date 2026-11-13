package services

import (
	"context"
	"encoding/json"
	"errors"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/uuid"
)

type BatchRequest struct {
	ServiceIDs []string        `json:"service_ids"`
	Action     string          `json:"action"`
	GroupID    json.RawMessage `json:"group_id"`
	Confirm    bool            `json:"confirm"`
}
type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type BatchResult struct {
	ServiceID string      `json:"service_id"`
	Status    string      `json:"status"`
	Error     *BatchError `json:"error,omitempty"`
}

func (r BatchRequest) validate() (Patch, error) {
	if len(r.ServiceIDs) < 1 || len(r.ServiceIDs) > 100 {
		return Patch{}, validation.Invalid("service_ids", "must contain 1 to 100 unique service UUIDs")
	}
	seen := make(map[string]bool, len(r.ServiceIDs))
	for _, id := range r.ServiceIDs {
		if !uuid.ValidUUID(id) || seen[id] {
			return Patch{}, validation.Invalid("service_ids", "must contain unique service UUIDs")
		}
		seen[id] = true
	}
	var patch Patch
	switch r.Action {
	case "pause", "resume":
		enabled := r.Action == "resume"
		patch.Enabled = &enabled
	case "move":
		var id *string
		if r.GroupID == nil || json.Unmarshal(r.GroupID, &id) != nil || id != nil && !uuid.ValidUUID(*id) {
			return patch, validation.Invalid("group_id", "must supply a group UUID or null for move")
		}
		patch.GroupID = r.GroupID
	case "delete":
		if !r.Confirm {
			return patch, validation.Invalid("confirm", "must be true for delete")
		}
	default:
		return patch, validation.Invalid("action", "must be one of pause, resume, move, delete")
	}
	if r.Action != "move" && r.GroupID != nil {
		return patch, validation.Invalid("group_id", "only allowed for move")
	}
	return patch, nil
}

func (m *Manager) Batch(ctx context.Context, req BatchRequest) ([]BatchResult, error) {
	patch, err := req.validate()
	if err != nil {
		return nil, err
	}
	if req.Action == "move" {
		var id *string
		_ = json.Unmarshal(req.GroupID, &id)
		if id != nil {
			if _, err := m.Group(ctx, *id); err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, validation.Invalid("group_id", "group does not exist")
				}
				return nil, err
			}
		}
	}
	results := make([]BatchResult, 0, len(req.ServiceIDs))
	// Like nodes/batch, even unchanged actions write the normal per-item audit.
	// Each save/delete owns its transaction; publish one scheduler reload after all items.
	defer m.signal()
	for _, id := range req.ServiceIDs {
		if req.Action == "delete" {
			err = m.delete(ctx, id, false)
		} else {
			_, err = m.save(ctx, id, patch, false, true)
		}
		result := BatchResult{ServiceID: id, Status: "succeeded"}
		if err != nil {
			result.Status = "failed"
			result.Error = &BatchError{Code: "storage_error", Message: "Service action failed"}
			if errors.Is(err, ErrNotFound) {
				result.Error = &BatchError{Code: "service_not_found", Message: "Service not found"}
			} else if storage.IsBusy(err) {
				result.Error = &BatchError{Code: "storage_busy", Message: "Storage is busy"}
			}
		}
		results = append(results, result)
	}
	return results, nil
}
