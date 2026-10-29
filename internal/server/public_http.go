package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

var errSnapshotBusy = errors.New("public snapshot changed while reading")

// publicSnapshotVersion bounds projection retries and retains the version for
// the final post-serialization fence shared by HTTP, SSE and WebSocket.
func (h *handler) publicSnapshotVersion(ctx context.Context, limit, offset int) (ingest.PublicPage, uint64, error) {
	for range 3 {
		if err := h.publicPageAccess(); err != nil {
			return ingest.PublicPage{}, 0, err
		}
		version := h.nodes.PublicVisibilityVersion()
		page, err := h.ingest.PublicPage(ctx, h.nodes, limit, offset, h.liveInterval(), h.now)
		if disabled := h.publicPageAccess(); disabled != nil {
			return ingest.PublicPage{}, 0, disabled
		}
		if err != nil {
			return ingest.PublicPage{}, 0, err
		}
		if h.nodes.PublicVisibilityVersion() == version {
			return page, version, nil
		}
	}
	return ingest.PublicPage{}, 0, errSnapshotBusy
}

func (h *handler) publicNodes(w http.ResponseWriter, r *http.Request, requestID string) {
	limit, offset, err := pagination(r)
	if err != nil {
		h.serviceError(w, nodes.ErrInvalidInput, requestID)
		return
	}
	result, visibility, err := h.publicSnapshotVersion(r.Context(), limit, offset)
	if err != nil {
		if errors.Is(err, errPublicPageDisabled) {
			h.publicPageError(w, requestID)
			return
		}
		if errors.Is(err, errSnapshotBusy) {
			w.Header().Set("Retry-After", strconv.Itoa(h.liveSeconds()))
			fail(w, http.StatusServiceUnavailable, "snapshot_busy", "Public snapshot is changing; retry later", requestID)
			return
		}
		h.serviceError(w, err, requestID)
		return
	}
	frame := snapshotFrame(h, result)
	body, err := json.Marshal(frame)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	if h.publicPageAccess() != nil {
		h.publicPageError(w, requestID)
		return
	}
	if h.nodes.PublicVisibilityVersion() != visibility {
		w.Header().Set("Retry-After", strconv.Itoa(h.liveSeconds()))
		fail(w, http.StatusServiceUnavailable, "snapshot_busy", "Public snapshot is changing; retry later", requestID)
		return
	}
	writeJSONBody(w, http.StatusOK, body)
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
