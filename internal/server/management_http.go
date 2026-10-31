package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"orbitbeacon/internal/server/auth"
)

func (h *handler) changePassword(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session) {
	var input struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	if err := h.auth.ChangePassword(r.Context(), session.User.ID, input.OldPassword, input.NewPassword); err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) auditEvents(w http.ResponseWriter, r *http.Request, requestID string) {
	limit, offset, before, err := paginationQuery(r, true)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	page, err := h.management.Events(r.Context(), limit, offset, before)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *handler) users(w http.ResponseWriter, r *http.Request, requestID string) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
	if r.URL.Path == "/api/v1/users" {
		if r.Method == http.MethodGet {
			users, err := h.auth.ListUsers(r.Context())
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"users": users})
			return
		}
		var input struct {
			Username           string          `json:"username"`
			Password           string          `json:"password"`
			MustChangePassword json.RawMessage `json:"must_change_password"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		var mustChangePassword bool
		if len(input.MustChangePassword) != 0 {
			if bytes.Equal(bytes.TrimSpace(input.MustChangePassword), []byte("null")) || json.Unmarshal(input.MustChangePassword, &mustChangePassword) != nil {
				fail(w, http.StatusBadRequest, "invalid_json", "must_change_password must be a boolean", requestID)
				return
			}
		}
		user, err := h.auth.CreateUser(r.Context(), input.Username, input.Password, mustChangePassword)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusCreated, user)
		return
	}
	if r.Method == http.MethodDelete {
		if err := h.auth.DeleteUser(r.Context(), id); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var user auth.User
	var err error
	if r.Method == http.MethodPatch {
		var patch auth.UserPatch
		if !decode(w, r, &patch, requestID) {
			return
		}
		user, err = h.auth.PatchUser(r.Context(), id, patch)
	} else {
		user, err = h.auth.GetUser(r.Context(), id)
	}
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (h *handler) groups(w http.ResponseWriter, r *http.Request, requestID string) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/groups/")
	collection := r.URL.Path == "/api/v1/groups"
	if collection && r.Method == http.MethodGet {
		groups, err := h.management.Groups(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
		return
	}
	if r.Method == http.MethodGet {
		group, err := h.management.Group(r.Context(), id)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, group)
		return
	}
	if r.Method == http.MethodDelete {
		if err := h.management.DeleteGroup(r.Context(), id); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	status := http.StatusOK
	if collection {
		id = ""
		status = http.StatusCreated
	}
	group, err := h.management.SaveGroup(r.Context(), id, input.Name)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, status, group)
}
