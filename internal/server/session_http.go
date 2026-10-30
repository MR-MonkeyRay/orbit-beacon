package server

import (
	"net/http"
	"strings"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
)

func (h *handler) loginSession(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Header.Get("Origin") != h.origin {
		fail(w, http.StatusForbidden, "forbidden", "A trusted Origin is required", requestID)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	session, err := h.auth.Login(auth.WithLoginPeer(r.Context(), h.clientAddr(r)), input.Username, input.Password)
	if err != nil {
		h.logLoginRejection(err, r, requestID)
		h.serviceError(w, err, requestID)
		return
	}
	http.SetCookie(w, h.cookie(session.Token, session.ExpiresAt, int(auth.SessionLifetime.Seconds())))
	writeJSON(w, http.StatusOK, session)
}

func (h *handler) sessionGate(w http.ResponseWriter, r *http.Request, requestID string, level authLevel) (*http.Request, string, auth.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		fail(w, http.StatusUnauthorized, "unauthenticated", "A valid session is required", requestID)
		return nil, "", auth.Session{}, false
	}
	session, err := h.auth.CurrentSession(r.Context(), c.Value)
	if err != nil {
		h.serviceError(w, err, requestID)
		return nil, "", auth.Session{}, false
	}
	if session.User.MustChangePassword && level != pendingPasswordAllowed {
		h.serviceError(w, auth.ErrPasswordChangeRequired, requestID)
		return nil, "", auth.Session{}, false
	}
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
	if mutation {
		if r.Header.Get("Origin") != h.origin {
			fail(w, http.StatusForbidden, "forbidden", "A trusted Origin is required", requestID)
			return nil, "", auth.Session{}, false
		}
		valid, err := h.auth.VerifyCSRF(r.Context(), c.Value, r.Header.Get("X-CSRF-Token"))
		if err != nil {
			h.serviceError(w, err, requestID)
			return nil, "", auth.Session{}, false
		}
		if !valid {
			fail(w, http.StatusForbidden, "forbidden", "A session-bound CSRF token is required", requestID)
			return nil, "", auth.Session{}, false
		}
	}
	r = r.WithContext(audit.WithActor(r.Context(), session.User.ID))
	return r, c.Value, session, true
}

func (h *handler) logoutSession(w http.ResponseWriter, r *http.Request, requestID, token string) {
	if err := h.auth.Logout(r.Context(), token); err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	// A delayed logout must not expire a newer login's cookie.
	// Revocation above remains authoritative for the request's session.
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) cookie(value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", Secure: strings.HasPrefix(h.origin, "https://"), HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: maxAge}
}
