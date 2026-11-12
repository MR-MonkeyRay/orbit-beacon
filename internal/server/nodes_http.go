package server

import (
	"net/http"
	"strings"

	"orbitbeacon/internal/server/nodes"
)

type nodePage struct {
	HasMore    bool `json:"has_more"`
	NextOffset *int `json:"next_offset,omitempty"`
}

func (h *handler) readNodePage(w http.ResponseWriter, r *http.Request, requestID string) ([]nodes.Node, nodePage, bool) {
	limit, offset, err := pagination(r)
	if err != nil {
		h.serviceError(w, nodes.ErrInvalidInput, requestID)
		return nil, nodePage{}, false
	}
	list, hasMore, err := h.nodes.ListPage(r.Context(), limit, offset)
	if err != nil {
		h.serviceError(w, err, requestID)
		return nil, nodePage{}, false
	}
	page := nodePage{HasMore: hasMore}
	if next := offset + limit; hasMore && next > offset {
		page.NextOffset = &next
	}
	return list, page, true
}

func (h *handler) nodesRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	var err error
	switch r.URL.Path {
	case "/api/v1/nodes":
		if r.Method == http.MethodGet {
			if list, page, ok := h.readNodePage(w, r, requestID); ok {
				writeJSON(w, http.StatusOK, struct {
					Nodes []nodes.Node `json:"nodes"`
					nodePage
				}{list, page})
			}
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
		if len(parts) == 3 && parts[2] == "availability" {
			h.nodeAvailability(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "traffic" {
			h.nodeTraffic(w, r, id, requestID)
			return
		}
		if len(parts) == 4 && parts[2] == "probes" && parts[3] == "history" {
			h.nodeProbeHistory(w, r, id, requestID)
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
