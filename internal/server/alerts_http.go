package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/nodes"
)

func (h *handler) alertsRoute(w http.ResponseWriter, r *http.Request, requestID string) bool {
	p := r.URL.Path
	if p == "/api/v1/alerts/summary" {
		summary, err := h.alerts.Summary(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
		} else {
			writeJSON(w, http.StatusOK, summary)
		}
		return true
	}
	if strings.HasPrefix(p, "/api/v1/alerts/") {
		if r.URL.RawQuery != "" {
			h.serviceError(w, &nodes.ValidationError{Fields: map[string]string{"query": "unknown parameter"}}, requestID)
			return true
		}
		event, err := h.alerts.Event(r.Context(), strings.TrimPrefix(p, "/api/v1/alerts/"))
		if err != nil {
			h.serviceError(w, err, requestID)
		} else {
			writeJSON(w, http.StatusOK, event)
		}
		return true
	}
	if p == "/api/v1/alerts" {
		query, err := url.ParseQuery(r.URL.RawQuery)
		fields := map[string]string{}
		if err != nil {
			fields["query"] = "invalid query string"
		}
		for key, values := range query {
			if key != "state" && key != "limit" && key != "before" {
				fields[key] = "unknown parameter"
			} else if len(values) != 1 || values[0] == "" {
				fields[key] = "must be supplied once and nonempty"
			}
		}
		limit := 50
		if query.Has("limit") {
			limit, err = strconv.Atoi(query.Get("limit"))
			if err != nil {
				fields["limit"] = "must be an integer"
			}
		}
		if len(fields) > 0 {
			h.serviceError(w, &nodes.ValidationError{Fields: fields}, requestID)
			return true
		}
		page, err := h.alerts.Events(r.Context(), query.Get("state"), limit, query.Get("before"))
		if err != nil {
			h.serviceError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusOK, page)
		return true
	}
	if p != "/api/v1/alert-rules" && !strings.HasPrefix(p, "/api/v1/alert-rules/") {
		return false
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
		return true
	}
	if r.Method == http.MethodDelete {
		if err := h.alerts.Delete(r.Context(), id); err != nil {
			h.serviceError(w, err, requestID)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return true
	}
	var patch alerts.Patch
	if !decode(w, r, &patch, requestID) {
		return true
	}
	rule, err := h.alerts.Save(r.Context(), id, patch)
	if err != nil {
		h.serviceError(w, err, requestID)
		return true
	}
	status := http.StatusOK
	if collection {
		status = http.StatusCreated
	}
	writeJSON(w, status, rule)
	return true
}
