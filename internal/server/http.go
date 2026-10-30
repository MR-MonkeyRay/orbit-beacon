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
	if healthProbe(r.URL.Path) || strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		apiRoutes.serve(h, w, r, requestID)
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
