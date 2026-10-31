package server

import (
	"net/http"
	"strings"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func (h *handler) nodesRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	var err error
	switch r.URL.Path {
	case "/api/v1/nodes":
		if r.Method == http.MethodGet {
			limit, offset, err := pagination(r)
			if err != nil {
				h.serviceError(w, nodes.ErrInvalidInput, requestID)
				return
			}
			list, hasMore, err := h.nodes.ListPage(r.Context(), limit, offset)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			var nextOffset *int
			if hasMore {
				next := offset + limit
				if next > offset {
					nextOffset = &next
				}
			}
			writeJSON(w, http.StatusOK, struct {
				Nodes      []nodes.Node `json:"nodes"`
				HasMore    bool         `json:"has_more"`
				NextOffset *int         `json:"next_offset,omitempty"`
			}{Nodes: list, HasMore: hasMore, NextOffset: nextOffset})
			return
		}
		var input nodes.CreateRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		node, err := h.nodes.CreatePending(r.Context(), input)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusCreated, node)
	case "/api/v1/nodes/batch":
		var input nodes.BatchRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		results, err := h.nodes.Batch(r.Context(), input)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Results []nodes.BatchResult `json:"results"`
		}{Results: results})
	default:
		id := parts[1]
		if len(parts) == 3 && parts[2] == "history" {
			h.nodeHistory(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "traffic" {
			h.nodeTraffic(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "probes" {
			h.nodeProbes(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "latest" {
			h.nodeLatest(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "public-settings" {
			h.publicSettings(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "credential" {
			if err := h.nodes.RevokeCredential(r.Context(), id); err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(parts) == 3 && parts[2] == "runtime" {
			runtime, err := h.nodes.GetRuntime(r.Context(), id)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, struct {
				Runtime *v1.RuntimeSettings `json:"runtime"`
			}{runtime})
			return
		}
		if len(parts) == 3 && parts[2] == "traffic-settings" {
			var settings nodes.TrafficSettings
			if r.Method == http.MethodGet {
				settings, err = h.nodes.GetTrafficSettings(r.Context(), id)
			} else {
				var patch nodes.TrafficSettingsPatch
				if !decode(w, r, &patch, requestID) {
					return
				}
				settings, err = h.nodes.PatchTrafficSettings(r.Context(), id, patch)
			}
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, settings)
			return
		}
		if r.Method == http.MethodPatch {
			var patch nodes.MetadataPatch
			if !decode(w, r, &patch, requestID) {
				return
			}
			node, err := h.nodes.PatchMetadata(r.Context(), id, patch)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, node)
			return
		}
		if r.Method == http.MethodDelete {
			if err := h.nodes.Delete(r.Context(), id, r.URL.Query().Get("confirm") == "true"); err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		node, err := h.nodes.Get(r.Context(), id)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, node)
	}
}
