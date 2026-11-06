package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/validation"
)

func (h *handler) apiTokens(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session, sessionToken string) {
	switch r.Method {
	case http.MethodGet:
		tokens, err := h.auth.ListAPITokens(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"api_tokens": tokens})
	case http.MethodPost:
		var input struct {
			Name          string          `json:"name"`
			ExpiresInDays json.RawMessage `json:"expires_in_days"`
			Password      string          `json:"password"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		fields := map[string]string{}
		if strings.TrimSpace(input.Name) == "" || len(input.Name) > 100 {
			fields["name"] = "Enter a name of at most 100 bytes."
		}
		var days *int
		if len(input.ExpiresInDays) == 0 || json.Unmarshal(input.ExpiresInDays, &days) != nil || (days != nil && *days != 30 && *days != 90 && *days != 365) {
			fields["expires_in_days"] = "must be 30, 90, 365 or null"
		}
		if len(fields) > 0 {
			h.serviceError(w, &validation.Error{Fields: fields}, requestID)
			return
		}
		metadata, token, err := h.auth.CreateAPIToken(r.Context(), session.User.ID, sessionToken, input.Name, days, input.Password)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusCreated, struct {
			auth.APIToken
			Token string `json:"token"`
		}{metadata, token})
	case http.MethodPatch:
		limit, ok := h.decodeTokenRateLimit(w, r, requestID, true)
		if !ok {
			return
		}
		if err := h.auth.SetAPITokenRateLimit(r.Context(), strings.TrimPrefix(r.URL.Path, "/api/v1/api-tokens/"), limit); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rate_limit_per_minute": limit})
	case http.MethodDelete:
		if err := h.auth.RevokeAPIToken(r.Context(), strings.TrimPrefix(r.URL.Path, "/api/v1/api-tokens/")); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) apiTokenSettings(w http.ResponseWriter, r *http.Request, requestID string, _ auth.Session, _ string) {
	if r.Method == http.MethodPatch {
		limit, ok := h.decodeTokenRateLimit(w, r, requestID, false)
		if !ok {
			return
		}
		if err := h.auth.SetGlobalAPITokenRateLimit(r.Context(), *limit); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
	}
	limit, err := h.auth.APITokenRateLimit(r.Context())
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"rate_limit_per_minute": limit})
}

func (h *handler) decodeTokenRateLimit(w http.ResponseWriter, r *http.Request, requestID string, nullable bool) (*int, bool) {
	var input map[string]json.RawMessage
	if !decode(w, r, &input, requestID) {
		return nil, false
	}
	fields := map[string]string{}
	for key := range input {
		if key != "rate_limit_per_minute" {
			fields[key] = "unknown field"
		}
	}
	raw, exists := input["rate_limit_per_minute"]
	var limit *int
	if !exists {
		fields["rate_limit_per_minute"] = "is required"
	} else if json.Unmarshal(raw, &limit) != nil || (limit == nil && !nullable) || (limit != nil && (*limit < 1 || *limit > 100000)) {
		fields["rate_limit_per_minute"] = "must be an integer between 1 and 100000"
	}
	if len(fields) > 0 {
		h.serviceError(w, &validation.Error{Fields: fields}, requestID)
		return nil, false
	}
	return limit, true
}
