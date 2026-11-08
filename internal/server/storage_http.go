package server

import (
	"log/slog"
	"net/http"
	"strings"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/uuid"
)

func (h *handler) storageRoute(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/api/v1/storage":
		status, err := h.storage.RecoveryStatus(r.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		if h.notifications != nil {
			settings, err := h.notifications.StorageNotificationSettings(r.Context())
			if err != nil {
				if r.Context().Err() != nil || !h.nodes.Database().Ready() {
					h.serviceError(w, storage.ErrUnavailable, requestID)
					return
				}
				status.NotificationSettings.SnapshotReady = false
				h.logger.LogAttrs(r.Context(), slog.LevelWarn, "storage_notification_settings_unavailable", logging.ErrorAttrs(err, "storage_error")...)
			} else {
				status.NotificationSettings = settings
			}
		}
		writeJSON(w, http.StatusOK, status)
	case "/api/v1/storage/notifications":
		var input struct {
			Inbox      *bool    `json:"inbox"`
			ChannelIDs []string `json:"channel_ids"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		if input.Inbox == nil || input.ChannelIDs == nil {
			h.serviceError(w, validation.Invalid("notifications", "inbox and channel_ids are required and must not be null"), requestID)
			return
		}
		if h.notifications == nil {
			h.serviceError(w, storage.ErrUnavailable, requestID)
			return
		}
		settings, err := h.notifications.UpdateStorageNotificationSettings(r.Context(), storage.StorageNotificationPreferences{Inbox: *input.Inbox, ChannelIDs: input.ChannelIDs})
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, settings)
	case "/api/v1/storage/operations/preview":
		var input storage.RecoveryRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		if input.Domain != storage.MainDomain && input.Domain != storage.HistoryDomain {
			h.serviceError(w, validation.Invalid("domain", "must be main or history"), requestID)
			return
		}
		if input.Action != "rebuild" && input.Action != "resume" && input.Action != "promote" {
			h.serviceError(w, validation.Invalid("action", "must be rebuild, resume or promote"), requestID)
			return
		}
		if input.Target != "primary" && input.Target != "standby" {
			h.serviceError(w, validation.Invalid("target", "must be primary or standby"), requestID)
			return
		}
		preview, err := h.storage.PreviewRecovery(r.Context(), session.User.ID, input)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	default:
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/storage/operations/")
		if !uuid.ValidUUID(id) {
			fail(w, http.StatusNotFound, "not_found", "Recovery operation not found", requestID)
			return
		}
		if r.Method == http.MethodGet {
			operation, err := h.storage.RecoveryOperation(r.Context(), session.User.ID, id)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, operation)
			return
		}
		var input struct {
			Confirmation   *string `json:"confirmation"`
			AcceptDataLoss *bool   `json:"accept_data_loss"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		if input.Confirmation == nil || *input.Confirmation == "" || input.AcceptDataLoss == nil {
			h.serviceError(w, validation.Invalid("operation", "confirmation and accept_data_loss are required and must not be null"), requestID)
			return
		}
		lease, err := h.nodes.Database().Acquire(r.Context(), storage.Writer)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		defer lease.Close()
		guard, err := lease.BeginWrite(lease.Context())
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		defer guard.Rollback()
		checked, _, current, ok := h.sessionGate(w, r.WithContext(lease.Context()), requestID, admin)
		if !ok {
			return
		}
		operation, err := h.storage.AcceptRecovery(checked.Context(), current.User.ID, id, *input.Confirmation, *input.AcceptDataLoss)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		w.Header().Set("Location", "/api/v1/storage/operations/"+operation.ID)
		writeJSON(w, http.StatusAccepted, operation)
	}
}
