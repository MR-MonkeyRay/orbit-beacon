package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/auth"
)

var (
	readMethods           = []string{http.MethodGet}
	createMethods         = []string{http.MethodGet, http.MethodPost}
	itemMethods           = []string{http.MethodGet, http.MethodPatch, http.MethodDelete}
	configMethods         = []string{http.MethodGet, http.MethodPatch}
	postMethods           = []string{http.MethodPost}
	patchMethods          = []string{http.MethodPatch}
	sessionMethods        = []string{http.MethodPost, http.MethodDelete}
	enrollmentItemMethods = []string{http.MethodGet, http.MethodDelete}
	deleteMethods         = []string{http.MethodDelete}
)

// routeMethods establishes route existence and method support before session
// authentication, role, Origin or CSRF checks.
func routeMethods(p string) []string {
	switch p {
	case "/api/v1/session":
		return sessionMethods
	case "/api/v1/me", "/api/v1/audit", "/api/v1/alerts", "/api/v1/alerts/summary", "/api/v1/notification-deliveries", "/api/public/v1/nodes", "/api/public/v1/stream":
		return readMethods
	case "/api/v1/me/password":
		return patchMethods
	case "/api/v1/users", "/api/v1/groups", "/api/v1/nodes", "/api/v1/alert-rules", "/api/v1/notification-channels":
		return createMethods
	case "/api/v1/nodes/batch", "/api/agent/v1/enroll", "/api/agent/v1/enroll/check", "/api/agent/v1/report", "/api/agent/v1/recover":
		return postMethods
	case "/api/v1/enrollments":
		return createMethods
	}
	if !strings.HasPrefix(p, "/api/v1/") {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/api/v1/"), "/")
	if len(parts) < 2 || !identity.ValidUUID(parts[1]) {
		return nil
	}
	if len(parts) == 2 && parts[0] == "enrollments" {
		return enrollmentItemMethods
	}
	if len(parts) == 2 && (parts[0] == "notification-channels" || parts[0] == "alert-rules" || parts[0] == "users" || parts[0] == "groups" || parts[0] == "nodes") {
		return itemMethods
	}
	if len(parts) == 3 && parts[0] == "notification-channels" && parts[2] == "test" {
		return postMethods
	}
	if len(parts) == 3 && parts[0] == "nodes" && (parts[2] == "config" || parts[2] == "public-settings") {
		return configMethods
	}
	if len(parts) == 3 && parts[0] == "nodes" && (parts[2] == "latest" || parts[2] == "history" || parts[2] == "traffic" || parts[2] == "probes") {
		return readMethods
	}
	if len(parts) == 3 && parts[0] == "nodes" && parts[2] == "credential" {
		return deleteMethods
	}
	return nil
}

func (h *handler) managementRoute(w http.ResponseWriter, r *http.Request, requestID string, user auth.User) bool {
	p := r.URL.Path
	switch {
	case p == "/api/v1/me/password":
		var input struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
		}
		if !decode(w, r, &input, requestID) {
			return true
		}
		if err := h.auth.ChangePassword(r.Context(), user.ID, input.OldPassword, input.NewPassword); err != nil {
			h.serviceError(w, err, requestID)
			return true
		}
		http.SetCookie(w, cookie("", time.Unix(1, 0), -1))
		w.WriteHeader(http.StatusNoContent)
	case p == "/api/v1/users" || strings.HasPrefix(p, "/api/v1/users/"):
		h.users(w, r, requestID)
	case p == "/api/v1/groups" || strings.HasPrefix(p, "/api/v1/groups/"):
		h.groups(w, r, requestID)
	case p == "/api/v1/audit":
		limit, offset, err := pagination(r)
		if err != nil {
			h.serviceError(w, auth.ErrInvalidInput, requestID)
			return true
		}
		before := r.URL.Query().Get("before")
		if r.URL.Query().Has("before") && before == "" {
			h.serviceError(w, auth.ErrInvalidInput, requestID)
			return true
		}
		page, err := h.management.Events(r.Context(), limit, offset, before)
		if err != nil {
			h.serviceError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusOK, page)
	default:
		return false
	}
	return true
}

func pagination(r *http.Request) (int, int, error) {
	limit, offset := 50, 0
	var err error
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			return 0, 0, err
		}
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil {
			return 0, 0, err
		}
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return 0, 0, auth.ErrInvalidInput
	}
	return limit, offset, nil
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
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		user, err := h.auth.CreateUser(r.Context(), input.Username, input.Password, input.Role)
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
