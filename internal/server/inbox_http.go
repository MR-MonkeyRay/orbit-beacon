package server

import (
	"net/http"
	"strings"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/uuid"
)

func (h *handler) inboxRoute(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session) {
	const collection = "/api/v1/notifications"
	path := r.URL.Path
	if h.inbox == nil {
		fail(w, http.StatusServiceUnavailable, "not_available", "Inbox service is unavailable", requestID)
		return
	}
	filter, err := inboxQuery(r, path == collection)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	who := inbox.Identity{UserID: session.User.ID}
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
			return
		}
		if !uuid.ValidUUID(body.Through) {
			h.serviceError(w, &validation.Error{Fields: map[string]string{"through": "must be a notification UUID"}}, requestID)
			return
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
				return
			}
			if body.Read == nil || !*body.Read {
				h.serviceError(w, &validation.Error{Fields: map[string]string{"read": "must be true"}}, requestID)
				return
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
}

func inboxQuery(r *http.Request, list bool) (inbox.Filter, error) {
	filter := inbox.Filter{Limit: 50}
	allowed := map[string]queryParameter{}
	if list {
		const suppliedOnce = "must be supplied once and nonempty"
		allowed = map[string]queryParameter{
			"limit":  {nonempty: true, repeated: suppliedOnce},
			"before": {nonempty: true, repeated: suppliedOnce},
			"unread": {nonempty: true, repeated: suppliedOnce},
		}
	}
	query, fields := parseQuery(r.URL.RawQuery, "invalid query string", allowed)
	if list {
		filter.Limit = parseLimit(query, fields, 50, "must be an integer from 1 to 100")
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
		return inbox.Filter{}, &validation.Error{Fields: fields}
	}
	return filter, nil
}
