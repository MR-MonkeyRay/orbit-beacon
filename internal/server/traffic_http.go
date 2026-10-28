package server

import (
	"net/http"
	"net/url"
	"strconv"

	"orbitbeacon/internal/server/nodes"
)

func (h *handler) nodeTraffic(w http.ResponseWriter, r *http.Request, id, requestID string) {
	days, fields := trafficDays(r.URL.RawQuery)
	if len(fields) > 0 {
		h.serviceError(w, &nodes.ValidationError{Fields: fields}, requestID)
		return
	}
	summary, err := h.traffic.Read(r.Context(), id, days, h.now())
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func trafficDays(rawQuery string) (int, map[string]string) {
	query, err := url.ParseQuery(rawQuery)
	fields := make(map[string]string)
	if err != nil {
		fields["query"] = "must be a valid query string"
	}
	for key, values := range query {
		if key != "days" {
			fields[key] = "unknown parameter"
		} else if len(values) != 1 {
			fields[key] = "must be supplied exactly once"
		}
	}
	days := 30
	if values, ok := query["days"]; ok && len(values) == 1 {
		parsed, parseErr := strconv.Atoi(values[0])
		if parseErr != nil || parsed < 1 || parsed > 400 {
			fields["days"] = "must be an integer from 1 to 400"
		} else {
			days = parsed
		}
	}
	return days, fields
}
