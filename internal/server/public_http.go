package server

import (
	"net/http"

	"orbitbeacon/internal/server/nodes"
)

func (h *handler) publicNodes(w http.ResponseWriter, r *http.Request, requestID string) {
	limit, offset, err := pagination(r)
	if err != nil {
		h.serviceError(w, nodes.ErrInvalidInput, requestID)
		return
	}
	result, err := h.ingest.PublicPage(r.Context(), h.nodes, limit, offset, h.now)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
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
