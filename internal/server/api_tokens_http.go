package server

import (
	"net/http"
	"net/url"
	"strings"

	"orbitbeacon/internal/server/auth"
)

type headResponseWriter struct{ http.ResponseWriter }

func (w headResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w headResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func containsGET(methods []string) bool {
	for _, method := range methods {
		if method == http.MethodGet {
			return true
		}
	}
	return false
}

// lenientQuery decodes every key/value it can, keeping undecodable parts
// verbatim, so one malformed escape cannot hide a credential elsewhere.
func lenientQuery(raw string) url.Values {
	values := url.Values{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '&' || r == ';' }) {
		key, value, _ := strings.Cut(part, "=")
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		values.Add(key, value)
	}
	return values
}

func tokenInQuery(r *http.Request) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		// Malformed queries are ordinary validation failures unless some part
		// could carry a token.
		query = lenientQuery(r.URL.RawQuery)
	}
	for key, values := range query {
		key, values := strings.ToLower(key), values
		if strings.Contains(key, "obt_") {
			return true
		}
		switch strings.ToLower(key) {
		case "token", "access_token", "api_token", "api-token", "authorization":
			return true
		}
		for _, value := range values {
			if strings.Contains(strings.ToLower(value), "obt_") {
				return true
			}
		}
	}
	return false
}

func apiTokenReadAllowed(p string) bool {
	if !strings.HasPrefix(p, "/api/v1/") {
		return false
	}
	tail := strings.TrimPrefix(p, "/api/v1/")
	// Prefix matching fails closed: a new route that shares a sensitive prefix
	// stays token-inaccessible until it is reviewed and allowed explicitly.
	for _, prefix := range []string{"me", "session", "users", "api-tokens", "enrollments"} {
		if strings.HasPrefix(tail, prefix) {
			return false
		}
	}
	return !(strings.HasPrefix(tail, "notification-channels") && strings.HasSuffix(tail, "/test"))
}

func (h *handler) apiTokensRoute(w http.ResponseWriter, r *http.Request, requestID string, user auth.User) bool {
	if r.URL.Path != "/api/v1/api-tokens" && !strings.HasPrefix(r.URL.Path, "/api/v1/api-tokens/") {
		return false
	}
	if user.Role != auth.RoleAdmin {
		fail(w, http.StatusForbidden, "forbidden", "Administrator role is required", requestID)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := h.auth.ListAPITokens(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"api_tokens": tokens})
	case http.MethodPost:
		var input struct {
			Name          string `json:"name"`
			ExpiresInDays *int   `json:"expires_in_days"`
		}
		if !decode(w, r, &input, requestID) {
			return true
		}
		token, err := h.auth.CreateAPIToken(r.Context(), user.ID, input.Name, input.ExpiresInDays)
		if err != nil {
			h.serviceError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusCreated, token)
	case http.MethodDelete:
		if err := h.auth.RevokeAPIToken(r.Context(), strings.TrimPrefix(r.URL.Path, "/api/v1/api-tokens/")); err != nil {
			h.serviceError(w, err, requestID)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	}
	return true
}
