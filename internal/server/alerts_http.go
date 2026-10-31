package server

import (
	"net/http"
	"strings"

	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/validation"
)

func (h *handler) alertsRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	p := r.URL.Path
	if p == "/api/v1/alerts/summary" {
		summary, err := h.alerts.Summary(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
		} else {
			writeJSON(w, http.StatusOK, summary)
		}
		return
	}
	if strings.HasPrefix(p, "/api/v1/alerts/") {
		if r.URL.RawQuery != "" {
			h.serviceError(w, &validation.Error{Fields: map[string]string{"query": "unknown parameter"}}, requestID)
			return
		}
		event, err := h.alerts.Event(r.Context(), strings.TrimPrefix(p, "/api/v1/alerts/"))
		if err != nil {
			h.serviceError(w, err, requestID)
		} else {
			writeJSON(w, http.StatusOK, event)
		}
		return
	}
	if p == "/api/v1/alerts" {
		const suppliedOnce = "must be supplied once and nonempty"
		query, fields := parseQuery(r.URL.RawQuery, "invalid query string", map[string]queryParameter{
			"state":  {nonempty: true, repeated: suppliedOnce},
			"limit":  {nonempty: true, repeated: suppliedOnce},
			"before": {nonempty: true, repeated: suppliedOnce},
		})
		limit := parseLimit(query, fields, 50, "must be an integer")
		if len(fields) > 0 {
			h.serviceError(w, &validation.Error{Fields: fields}, requestID)
			return
		}
		page, err := h.alerts.Events(r.Context(), query.Get("state"), limit, query.Get("before"))
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	collection := p == "/api/v1/alert-rules"
	id := ""
	if !collection {
		id = strings.TrimPrefix(p, "/api/v1/alert-rules/")
	}
	if r.Method == http.MethodGet {
		if collection {
			rules, err := h.alerts.List(r.Context())
			if err != nil {
				h.serviceError(w, err, requestID)
			} else {
				writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
			}
		} else {
			rule, err := h.alerts.Get(r.Context(), id)
			if err != nil {
				h.serviceError(w, err, requestID)
			} else {
				writeJSON(w, http.StatusOK, rule)
			}
		}
		return
	}
	if r.Method == http.MethodDelete {
		if err := h.alerts.Delete(r.Context(), id); err != nil {
			h.serviceError(w, err, requestID)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	var patch alerts.Patch
	if !decode(w, r, &patch, requestID) {
		return
	}
	rule, err := h.alerts.Save(r.Context(), id, patch)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	status := http.StatusOK
	if collection {
		status = http.StatusCreated
	}
	writeJSON(w, status, rule)
}
