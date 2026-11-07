package server

import (
	"errors"
	"net/http"
	"strings"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/commands"
)

func (h *handler) commandsRoute(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session) {
	parts := strings.Split(r.URL.Path, "/")
	nodeID := parts[4]
	if len(parts) == 8 {
		if err := h.commands.Cancel(r.Context(), nodeID, parts[6]); err != nil {
			h.commandError(w, err, requestID)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodGet {
		records, err := h.commands.List(r.Context(), nodeID)
		if err != nil {
			h.commandError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Commands []commands.Record `json:"commands"`
		}{records})
		return
	}
	var req commands.CreateRequest
	if !decode(w, r, &req, requestID) {
		return
	}
	record, err := h.commands.Create(r.Context(), nodeID, session.User.Username, req)
	if err != nil {
		h.commandError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}
func (h *handler) commandError(w http.ResponseWriter, err error, requestID string) {
	for _, mapping := range []struct {
		err    error
		status int
		code   string
	}{
		{commands.ErrSameVersion, http.StatusConflict, "command_same_version"},
		{commands.ErrInvalid, http.StatusBadRequest, "validation_failed"}, {commands.ErrNotFound, http.StatusNotFound, "command_not_found"},
		{commands.ErrNotAllowed, http.StatusConflict, "command_not_allowed"}, {commands.ErrActive, http.StatusConflict, "command_active"},
		{commands.ErrNodeUnavailable, http.StatusConflict, "node_unavailable"}, {commands.ErrNotPending, http.StatusConflict, "command_not_pending"},
	} {
		if errors.Is(err, mapping.err) {
			fail(w, mapping.status, mapping.code, "Command request could not be completed", requestID)
			return
		}
	}
	h.serviceError(w, err, requestID)
}

func (h *handler) groupCommandsRoute(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session) {
	groupID := strings.Split(r.URL.Path, "/")[4]
	// Keep group lookup errors consistent with the group management API.
	if _, err := h.management.Group(r.Context(), groupID); err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	var req commands.CreateRequest
	if !decode(w, r, &req, requestID) {
		return
	}
	result, err := h.commands.CreateGroup(r.Context(), groupID, session.User.Username, req)
	if err != nil {
		h.commandError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
