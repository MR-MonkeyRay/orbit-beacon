package server

import (
	"net/http"
	"strings"
)

func (h *handler) serviceGroups(w http.ResponseWriter, r *http.Request, requestID string) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/service-groups/")
	collection := r.URL.Path == "/api/v1/service-groups"
	if collection && r.Method == http.MethodGet {
		groups, err := h.services.Groups(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"service_groups": groups})
		return
	}
	if r.Method == http.MethodGet {
		group, err := h.services.Group(r.Context(), id)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, group)
		return
	}
	if r.Method == http.MethodDelete {
		if err := h.services.DeleteGroup(r.Context(), id); err != nil {
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
	group, err := h.services.SaveGroup(r.Context(), id, input.Name)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, status, group)
}

func (h *handler) serviceGroupOrder(w http.ResponseWriter, r *http.Request, requestID string) {
	var input struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	groups, err := h.services.ReorderGroups(r.Context(), input.IDs)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service_groups": groups})
}
