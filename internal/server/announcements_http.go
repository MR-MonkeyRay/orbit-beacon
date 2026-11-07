package server

import (
	"net/http"
	"strings"
	"time"

	"orbitbeacon/internal/server/announcements"
	"orbitbeacon/internal/server/nodes"
)

// 公告属于安全相关的公开内容，写入后活跃流要立即刷新。
func newAnnouncementService(ns *nodes.Service, now func() time.Time) *announcements.Service {
	return announcements.New(ns.Database(), now, ns.InvalidatePublicSafety)
}

func (h *handler) announcementsRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method == http.MethodGet {
		items, err := h.announcements.List(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"announcements": items})
		}
		return
	}
	id := ""
	if r.URL.Path != "/api/v1/announcements" {
		id = strings.TrimPrefix(r.URL.Path, "/api/v1/announcements/")
	}
	if r.Method == http.MethodDelete {
		if err := h.announcements.Delete(r.Context(), id); err != nil {
			h.serviceError(w, err, requestID)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	var patch announcements.Patch
	if !decode(w, r, &patch, requestID) {
		return
	}
	item, err := h.announcements.Save(r.Context(), id, patch)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, item)
}
