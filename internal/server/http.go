package server

import (
	"crypto/rand"
	"net/http"
	"strings"

	"orbitbeacon/internal/uuid"
)

type headResponseWriter struct{ http.ResponseWriter }

func (w headResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w headResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.observeRequest(w, r, h.serveHTTP)
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w = headResponseWriter{w}
	}
	requestID, err := uuid.UUID(rand.Reader)
	if err != nil {
		h.logInternalError(err, "")
		http.Error(w, "Unable to generate request identity", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if r.URL.Path == "/healthz/live" || r.URL.Path == "/healthz/ready" {
		h.health(w, r, requestID)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		h.api(w, r, requestID)
		return
	}
	h.static(w, r)
}

func method(w http.ResponseWriter, r *http.Request, requestID string, allowed ...string) bool {
	for _, candidate := range allowed {
		if r.Method == candidate {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not supported for this route", requestID)
	return false
}

func (h *handler) api(w http.ResponseWriter, r *http.Request, requestID string) {
	p := r.URL.Path
	allowed := routeMethods(p)
	if len(allowed) == 0 {
		fail(w, http.StatusNotFound, "not_found", "Route not found", requestID)
		return
	}
	if !method(w, r, requestID, allowed...) {
		return
	}
	if p == "/api/public/v1/nodes" || p == "/api/public/v1/stream" || p == "/api/public/v1/ws" {
		if err := h.publicPageAccess(); err != nil {
			h.publicPageError(w, requestID)
			return
		}
	}
	if p == "/api/public/v1/nodes" {
		h.publicNodes(w, r, requestID)
		return
	}
	if p == "/api/public/v1/stream" {
		h.publicStream(w, r, requestID)
		return
	}
	if p == "/api/public/v1/ws" {
		h.publicWebSocket(w, r, requestID)
		return
	}
	if strings.HasPrefix(p, "/api/agent/v1/") {
		h.agentRoute(w, r, requestID)
		return
	}
	if p == "/api/v1/session" && r.Method == http.MethodPost {
		h.loginSession(w, r, requestID)
		return
	}
	r, token, session, ok := h.sessionGate(w, r, requestID)
	if !ok {
		return
	}
	user := session.User
	if h.backupsRoute(w, r, requestID, user) || h.inboxRoute(w, r, requestID, user) {
		return
	}
	if h.notificationsRoute(w, r, requestID) {
		return
	}
	if h.alertsRoute(w, r, requestID) {
		return
	}
	if h.managementRoute(w, r, requestID, user) {
		return
	}
	if h.enrollmentRoute(w, r, requestID) {
		return
	}
	if p == "/api/v1/session" {
		h.logoutSession(w, r, requestID, token)
		return
	}
	h.nodesRoute(w, r, requestID)
}
