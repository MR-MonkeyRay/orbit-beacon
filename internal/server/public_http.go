package server

import (
	"errors"
	"net/http"
	"time"

	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

func (h *handler) publicNodes(w http.ResponseWriter, r *http.Request, requestID string) {
	limit, offset, err := pagination(r)
	if err != nil {
		h.serviceError(w, nodes.ErrInvalidInput, requestID)
		return
	}
	ids, more, err := h.nodes.ListPublicIDs(r.Context(), limit, offset)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	now := h.now().UTC()
	result := struct {
		Nodes       []ingest.PublicNode `json:"nodes"`
		HasMore     bool                `json:"has_more"`
		NextOffset  *int                `json:"next_offset"`
		GeneratedAt time.Time           `json:"generated_at"`
	}{Nodes: make([]ingest.PublicNode, 0, len(ids)), HasMore: more, GeneratedAt: now}
	if more {
		next := offset + len(ids)
		result.NextOffset = &next
	}
	for _, id := range ids {
		node, err := h.ingest.PublicNode(r.Context(), id, now)
		if errors.Is(err, nodes.ErrNotFound) {
			continue
		}
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		result.Nodes = append(result.Nodes, node)
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) publicSettings(w http.ResponseWriter, r *http.Request, id, requestID string) {
	var settings nodes.PublicSettings
	var err error
	if r.Method == http.MethodGet {
		settings, err = h.nodes.GetPublicSettings(r.Context(), id)
	} else {
		var patch nodes.PublicSettingsPatch
		if !decode(w, r, &patch, requestID) {
			return
		}
		settings, err = h.nodes.PatchPublicSettings(r.Context(), id, patch)
	}
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
