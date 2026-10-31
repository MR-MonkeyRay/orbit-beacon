package server

import (
	"net/http"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/validation"
)

func (h *handler) nodeHistory(w http.ResponseWriter, r *http.Request, id, requestID string) {
	query, fields := parseQuery(r.URL.RawQuery, "must be a valid query string", map[string]queryParameter{
		"metric": {repeated: "must be supplied exactly once"},
		"range":  {repeated: "must be supplied exactly once"},
	})
	metric, requestedRange := query.Get("metric"), query.Get("range")
	if !history.ValidMetric(metric) {
		fields["metric"] = "must be a supported history metric"
	}
	if _, err := history.SelectRange(requestedRange); err != nil {
		fields["range"] = "must be one of 1h, 6h, 24h, 7d, 30d"
	}
	if len(fields) > 0 {
		h.serviceError(w, &validation.Error{Fields: fields}, requestID)
		return
	}
	// 即使启动时禁用了历史记录，仍须确认节点存在。
	if _, err := h.nodes.Get(r.Context(), id); err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	response, _ := history.Empty(metric, requestedRange)
	if h.history != nil {
		var err error
		response, err = h.history.Query(r.Context(), id, metric, requestedRange, h.now())
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}
