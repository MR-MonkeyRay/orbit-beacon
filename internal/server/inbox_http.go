package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/nodes"
)

func (h *handler) inboxRoute(w http.ResponseWriter, r *http.Request, requestID string, user auth.User) bool {
	const collection = "/api/v1/notifications"
	path := r.URL.Path
	if path != collection && !strings.HasPrefix(path, collection+"/") {
		return false
	}
	if h.inbox == nil {
		fail(w, http.StatusServiceUnavailable, "not_available", "Inbox service is unavailable", requestID)
		return true
	}
	filter, err := inboxQuery(r, path == collection)
	if err != nil {
		h.serviceError(w, err, requestID)
		return true
	}
	who := inbox.Identity{UserID: user.ID}
	switch path {
	case collection:
		page, err := h.inbox.List(r.Context(), who, filter)
		if err != nil {
			h.serviceError(w, err, requestID)
		} else {
			writeJSON(w, http.StatusOK, page)
		}
	case collection + "/summary":
		summary, err := h.inbox.Summary(r.Context(), who)
		if err != nil {
			h.serviceError(w, err, requestID)
		} else {
			writeJSON(w, http.StatusOK, summary)
		}
	case collection + "/read-all":
		var body struct {
			Through string `json:"through"`
		}
		if !decode(w, r, &body, requestID) {
			return true
		}
		if !identity.ValidUUID(body.Through) {
			h.serviceError(w, &nodes.ValidationError{Fields: map[string]string{"through": "must be a notification UUID"}}, requestID)
			return true
		}
		if err := h.inbox.MarkAllRead(r.Context(), who, body.Through, h.now()); err != nil {
			h.serviceError(w, err, requestID)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		id := strings.TrimPrefix(path, collection+"/")
		if strings.HasSuffix(id, "/read") {
			id = strings.TrimSuffix(id, "/read")
			var body struct {
				Read *bool `json:"read"`
			}
			if !decode(w, r, &body, requestID) {
				return true
			}
			if body.Read == nil || !*body.Read {
				h.serviceError(w, &nodes.ValidationError{Fields: map[string]string{"read": "must be true"}}, requestID)
				return true
			}
			if err := h.inbox.MarkRead(r.Context(), who, id, h.now()); err != nil {
				h.serviceError(w, err, requestID)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		} else {
			notification, err := h.inbox.Detail(r.Context(), who, id)
			if err != nil {
				h.serviceError(w, err, requestID)
			} else {
				writeJSON(w, http.StatusOK, notification)
			}
		}
	}
	return true
}

func inboxQuery(r *http.Request, list bool) (inbox.Filter, error) {
	filter := inbox.Filter{Limit: 50}
	query, err := url.ParseQuery(r.URL.RawQuery)
	fields := make(map[string]string)
	if err != nil {
		fields["query"] = "invalid query string"
	}
	for key, values := range query {
		if !list || key != "limit" && key != "before" && key != "unread" {
			fields[key] = "unknown parameter"
		} else if len(values) != 1 || values[0] == "" {
			fields[key] = "must be supplied once and nonempty"
		}
	}
	if list {
		if query.Has("limit") {
			filter.Limit, err = strconv.Atoi(query.Get("limit"))
			if err != nil || filter.Limit < 1 || filter.Limit > 100 {
				fields["limit"] = "must be an integer from 1 to 100"
			}
		}
		filter.Before = query.Get("before")
		if query.Has("unread") {
			switch query.Get("unread") {
			case "true":
				filter.Unread = true
			case "false":
			default:
				fields["unread"] = "must be true or false"
			}
		}
	}
	if len(fields) != 0 {
		return inbox.Filter{}, &nodes.ValidationError{Fields: fields}
	}
	return filter, nil
}
