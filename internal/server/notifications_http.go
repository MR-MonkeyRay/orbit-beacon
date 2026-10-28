package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
)

func (h *handler) notificationsRoute(w http.ResponseWriter, r *http.Request, requestID string) bool {
	p := r.URL.Path
	if p != "/api/v1/notification-deliveries" && p != "/api/v1/notification-channels" && !strings.HasPrefix(p, "/api/v1/notification-channels/") {
		return false
	}
	if h.notifications == nil {
		fail(w, http.StatusServiceUnavailable, "not_available", "Notification service is unavailable", requestID)
		return true
	}
	if p == "/api/v1/notification-deliveries" {
		query, e := url.ParseQuery(r.URL.RawQuery)
		fields := map[string]string{}
		if e != nil {
			fields["query"] = "invalid query string"
		}
		for key, values := range query {
			if key != "limit" && key != "before" {
				fields[key] = "unknown parameter"
			} else if len(values) != 1 || values[0] == "" {
				fields[key] = "must be supplied once and nonempty"
			}
		}
		limit := 50
		if query.Has("limit") {
			limit, e = strconv.Atoi(query.Get("limit"))
			if e != nil {
				fields["limit"] = "must be an integer"
			}
		}
		if len(fields) > 0 {
			h.serviceError(w, &nodes.ValidationError{Fields: fields}, requestID)
			return true
		}
		page, e := h.notifications.Deliveries(r.Context(), limit, query.Get("before"))
		if e != nil {
			h.serviceError(w, e, requestID)
		} else {
			writeJSON(w, http.StatusOK, page)
		}
		return true
	}
	collection := p == "/api/v1/notification-channels"
	id := strings.TrimPrefix(p, "/api/v1/notification-channels/")
	if strings.HasSuffix(id, "/test") {
		result, e := h.notifications.Test(r.Context(), strings.TrimSuffix(id, "/test"))
		if e != nil {
			h.serviceError(w, e, requestID)
		} else {
			writeJSON(w, http.StatusOK, result)
		}
		return true
	}
	switch r.Method {
	case http.MethodGet:
		if collection {
			channels, e := h.notifications.List(r.Context())
			if e != nil {
				h.serviceError(w, e, requestID)
			} else {
				writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
			}
		} else {
			channel, e := h.notifications.Get(r.Context(), id)
			if e != nil {
				h.serviceError(w, e, requestID)
			} else {
				writeJSON(w, http.StatusOK, channel)
			}
		}
	case http.MethodDelete:
		if e := h.notifications.Delete(r.Context(), id); e != nil {
			h.serviceError(w, e, requestID)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		var patch notify.Patch
		if !decode(w, r, &patch, requestID) {
			return true
		}
		if collection {
			id = ""
		}
		channel, e := h.notifications.Save(r.Context(), id, patch)
		if e != nil {
			h.serviceError(w, e, requestID)
		} else {
			status := http.StatusOK
			if collection {
				status = http.StatusCreated
			}
			writeJSON(w, status, channel)
		}
	}
	return true
}
