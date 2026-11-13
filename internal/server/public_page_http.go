package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
)

var errPublicPageDisabled = errors.New("public page is disabled")

// publicPageSettingsInput 是 PATCH 请求体：字段可省略，出现时必须是布尔值，且至少给出一个。
type publicPageSettingsInput management.PublicPageSettingsPatch

func (p *publicPageSettingsInput) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, field := range []string{"enabled", "show_groups"} {
		if value, ok := raw[field]; ok {
			var boolean bool
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &boolean) != nil {
				return validation.Invalid(field, "must be a boolean")
			}
		}
	}
	var wire struct {
		Enabled    *bool `json:"enabled"`
		ShowGroups *bool `json:"show_groups"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if wire.Enabled == nil && wire.ShowGroups == nil {
		return validation.Invalid("enabled", "provide enabled or show_groups")
	}
	*p = publicPageSettingsInput{Enabled: wire.Enabled, ShowGroups: wire.ShowGroups}
	return nil
}

func (h *handler) publicPageAccess() error {
	if h.publicPageUnknown.Load() {
		return storage.ErrUnavailable
	}
	if h.publicPageDisabled.Load() {
		return errPublicPageDisabled
	}
	return nil
}

func (h *handler) publicPageError(w http.ResponseWriter, requestID string) {
	if h.publicPageUnknown.Load() {
		h.serviceError(w, storage.ErrUnavailable, requestID)
		return
	}
	fail(w, http.StatusForbidden, "public_page_disabled", "Public access is disabled", requestID)
}

func (h *handler) publicPageSettings(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method == http.MethodGet {
		h.publicPageMu.Lock()
		defer h.publicPageMu.Unlock()
		writeJSON(w, http.StatusOK, management.PublicPageSettings{Enabled: !h.publicPageDisabled.Load(), ShowGroups: h.publicPageShowGroups.Load()})
		return
	}
	var input publicPageSettingsInput
	if !decode(w, r, &input, requestID) {
		return
	}
	// 按提交顺序持久化、发布或撤回设置；即使事务已提交而 Store 尚未执行，
	// 读取也只使用已发布的运行时状态。
	h.publicPageMu.Lock()
	defer h.publicPageMu.Unlock()
	settings, err := h.management.PatchPublicPageSettings(r.Context(), management.PublicPageSettingsPatch(input))
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	enabledChanged := h.publicPageDisabled.Swap(!settings.Enabled) != !settings.Enabled
	groupsChanged := h.publicPageShowGroups.Swap(settings.ShowGroups) != settings.ShowGroups
	if enabledChanged || groupsChanged {
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
	if h.publicPageUnknown.Load() {
		h.serviceError(w, storage.ErrUnavailable, w.Header().Get("X-Request-ID"))
		return false
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
