package server_test

import (
	"net/http"
	"testing"
)

func TestWrongCurrentPasswordReturnsSpecificErrorAndPreservesSession(t *testing.T) {
	f := newHTTP(t, 2)
	session, csrf := login(t, f, "admin")

	response := request(t, f.handler, http.MethodPatch, "/api/v1/me/password", `{"old_password":"wrong current password","new_password":"new HTTP regression password"}`, session, csrf, publicURL)
	apiError(t, response, http.StatusBadRequest, "invalid_old_password")

	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", session, "", ""), http.StatusOK)
}
