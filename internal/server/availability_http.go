package server

import (
	"net/http"

	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/validation"
)

func (h *handler) nodeAvailability(w http.ResponseWriter, r *http.Request, id, requestID string) {
	if summary, ok := h.readAvailability(w, r, id, requestID); ok {
		writeJSON(w, http.StatusOK, summary)
	}
}

func (h *handler) readAvailability(w http.ResponseWriter, r *http.Request, id, requestID string) (availability.Summary, bool) {
	query, fields := parseQuery(r.URL.RawQuery, "must be a valid query string", map[string]queryParameter{"days": {repeated: "must be supplied exactly once"}})
	days := 30
	if values, ok := query["days"]; ok && len(values) == 1 {
		switch values[0] {
		case "30":
		case "90":
			days = 90
		default:
			fields["days"] = "must be 30 or 90"
		}
	}
	if len(fields) > 0 {
		h.serviceError(w, &validation.Error{Fields: fields}, requestID)
		return availability.Summary{}, false
	}
	summary, err := h.availability.Read(r.Context(), id, days, h.now())
	if err != nil {
		h.serviceError(w, err, requestID)
		return availability.Summary{}, false
	}
	return summary, true
}
