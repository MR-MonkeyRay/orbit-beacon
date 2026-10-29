package server

import (
	"errors"
	"net/http"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/management"
)

var errPublicPageDisabled = errors.New("public page is disabled")

func (h *handler) publicPageAccess() error {
	if h.publicPageDisabled.Load() {
		return errPublicPageDisabled
	}
	return nil
}

func (h *handler) publicPageError(w http.ResponseWriter, requestID string) {
	fail(w, http.StatusForbidden, "public_page_disabled", "Public access is disabled", requestID)
}

func (h *handler) publicPageSettings(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, management.PublicPageSettings{Enabled: !h.publicPageDisabled.Load()})
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	if input.Enabled == nil {
		fail(w, http.StatusBadRequest, "validation_failed", "enabled must be a boolean", requestID)
		return
	}
	// Keep persistence, publication and withdrawal in commit order. Reads use
	// only the published state, including the short transaction-to-Store gap.
	h.publicPageMu.Lock()
	defer h.publicPageMu.Unlock()
	settings, err := h.management.SetPublicPageEnabled(r.Context(), *input.Enabled)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	if h.publicPageDisabled.Swap(!settings.Enabled) != !settings.Enabled {
		h.nodes.InvalidatePublicSafety()
	}
	writeJSON(w, http.StatusOK, settings)
}

// publicPageRoot runs before asset mapping and conditional cache handling.
// An enabled public page is independent of every management session state.
func (h *handler) publicPageRoot(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if h.publicPageAccess() == nil {
		return true
	}
	target := "/login"
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		session, err := h.auth.CurrentSession(r.Context(), cookie.Value)
		if err != nil && !errors.Is(err, auth.ErrUnauthenticated) {
			h.serviceError(w, err, w.Header().Get("X-Request-ID"))
			return false
		}
		if err == nil {
			target = "/overview"
			if session.User.MustChangePassword {
				target = "/change-password"
			}
		}
	}
	http.Redirect(w, r, target, http.StatusFound)
	return false
}
