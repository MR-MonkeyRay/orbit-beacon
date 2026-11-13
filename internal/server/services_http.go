package server

import (
	"context"
	"net/http"
	"strings"

	"orbitbeacon/internal/server/services"
	"orbitbeacon/internal/server/validation"
)

func (h *handler) servicesRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.URL.Path == "/api/v1/services/batch" {
		if r.URL.RawQuery != "" {
			h.serviceError(w, validation.Invalid("query", "unknown parameter"), requestID)
			return
		}
		var input services.BatchRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		results, err := h.services.Batch(r.Context(), input)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/services"), "/")
	id := ""
	if len(parts) > 1 {
		id = parts[1]
	}
	if len(parts) > 2 {
		query, fields := parseQuery(r.URL.RawQuery, "invalid query string", map[string]queryParameter{"range": {nonempty: true, repeated: "must be supplied exactly once"}})
		if len(fields) > 0 {
			h.serviceError(w, &validation.Error{Fields: fields}, requestID)
			return
		}
		history, e := h.services.History(r.Context(), id, query.Get("range"))
		if e != nil {
			h.serviceError(w, e, requestID)
			return
		}
		writeJSON(w, http.StatusOK, history)
		return
	}
	if r.URL.RawQuery != "" {
		h.serviceError(w, validation.Invalid("query", "unknown parameter"), requestID)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if id == "" {
			list, e := h.services.List(r.Context())
			if e != nil {
				h.serviceError(w, e, requestID)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"services": list})
		} else {
			s, e := h.services.Detail(r.Context(), id)
			if e != nil {
				h.serviceError(w, e, requestID)
				return
			}
			writeJSON(w, http.StatusOK, s)
		}
	case http.MethodDelete:
		if e := h.services.Delete(r.Context(), id); e != nil {
			h.serviceError(w, e, requestID)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		var patch services.Patch
		if !decode(w, r, &patch, requestID) {
			return
		}
		s, e := h.services.Save(r.Context(), id, patch)
		if e != nil {
			h.serviceError(w, e, requestID)
			return
		}
		status := http.StatusOK
		if id == "" {
			status = http.StatusCreated
		}
		writeJSON(w, status, s)
	}
}
func (h *handler) RunServices(ctx context.Context) { h.services.Run(ctx) }
