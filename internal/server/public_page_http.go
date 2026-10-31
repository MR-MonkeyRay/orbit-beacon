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
	// 按提交顺序持久化、发布或撤回设置；即使事务已提交而 Store 尚未执行，
	// 读取也只使用已发布的运行时状态。
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

// publicPageRoot 在资源映射和条件缓存处理之前运行。
// 已启用的公开页独立于所有管理会话状态。
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
