package server

import (
	"net/http"
	"net/url"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
)

func (h *handler) nodeHistory(w http.ResponseWriter, r *http.Request, id, requestID string) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	fields := make(map[string]string)
	if err != nil {
		fields["query"] = "must be a valid query string"
	}
	for key, values := range query {
		if key != "metric" && key != "range" {
			fields[key] = "unknown parameter"
		} else if len(values) != 1 {
			fields[key] = "must be supplied exactly once"
		}
	}
	metric, requestedRange := query.Get("metric"), query.Get("range")
	if !history.ValidMetric(metric) {
		fields["metric"] = "must be a supported history metric"
	}
	if _, err := history.SelectRange(requestedRange); err != nil {
		fields["range"] = "must be one of 1h, 6h, 24h, 7d, 30d"
	}
	if len(fields) > 0 {
		h.serviceError(w, &nodes.ValidationError{Fields: fields}, requestID)
		return
	}
	// Existence is checked even when best-effort history was disabled at startup.
	if _, err := h.nodes.Get(r.Context(), id); err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	response, _ := history.Empty(metric, requestedRange)
	if h.history != nil {
		response, err = h.history.Query(r.Context(), id, metric, requestedRange, h.now())
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}
