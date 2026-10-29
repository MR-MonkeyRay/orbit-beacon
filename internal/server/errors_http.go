package server

import (
	"errors"
	"net/http"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/traffic"
	"orbitbeacon/internal/server/validation"
)

type errorScope uint8

const (
	serviceErrors errorScope = 1 << iota
	agentErrors
	backupErrors
	decodeErrors
)

type errorMapping struct {
	scopes  errorScope
	match   func(error) bool
	status  int
	code    string
	message string
	retry   bool
}

// Agent-specific responses precede the shared service mappings.
var errorMappings = []errorMapping{
	{agentErrors, matches(ingest.ErrProtocolUnsupported, v1.ErrProtocolUnsupported), http.StatusUpgradeRequired, "protocol_unsupported", "Protocol version is not supported", false},
	{agentErrors, matches(enrollment.ErrConflict), http.StatusConflict, "enrollment_conflict", "Enrollment does not match the bound installation and credential", false},
	{agentErrors, matches(enrollment.ErrExpired, enrollment.ErrRevoked, enrollment.ErrUnauthenticated), http.StatusUnauthorized, "unauthenticated", "Enrollment credential is invalid, expired or revoked", false},
	{agentErrors, matches(enrollment.ErrNotFound), http.StatusNotFound, "enrollment_not_found", "Enrollment not found", false},
	{agentErrors, matches(enrollment.ErrInvalidInput, ingest.ErrInvalidReport), http.StatusBadRequest, "validation_failed", "Request values are invalid", false},
	{agentErrors, matches(ingest.ErrIdentityConflict), http.StatusConflict, "identity_conflict", "A newer generation or another session owns this identity", false},
	{agentErrors, matches(enrollment.ErrRateLimited), http.StatusTooManyRequests, "rate_limited", "Enrollment rate limit exceeded", true},
	{agentErrors, matches(ingest.ErrRateLimited), http.StatusTooManyRequests, "rate_limited", "Report rate limit exceeded", true},
	{agentErrors, matches(ingest.ErrWriterBusy), http.StatusServiceUnavailable, "report_busy", "Report writer is busy", true},
	{agentErrors, matches(ingest.ErrServiceClosed), http.StatusServiceUnavailable, "service_unavailable", "Report ingestion is shutting down", true},
	{serviceErrors | agentErrors | backupErrors | decodeErrors, isFieldValidation, http.StatusBadRequest, "validation_failed", "", false},
	{decodeErrors, isBodyTooLarge, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds 64 KiB", false},
	{serviceErrors | agentErrors | backupErrors, storage.IsBusy, http.StatusServiceUnavailable, "storage_busy", "Storage is busy; retry later", true},
	{serviceErrors | agentErrors, matches(auth.ErrUnauthenticated, nodes.ErrUnauthenticated), http.StatusUnauthorized, "unauthenticated", "Credentials or session are invalid or expired", false},
	{serviceErrors | agentErrors, matches(auth.ErrWrongPassword), http.StatusBadRequest, "invalid_old_password", "Current password is incorrect", false},
	{serviceErrors | agentErrors, matches(auth.ErrPasswordChangeRequired), http.StatusForbidden, "password_change_required", "Change your password before accessing management", false},
	{serviceErrors | agentErrors, matches(auth.ErrPasswordUnchanged), http.StatusBadRequest, "password_unchanged", "New password must differ from the current password", false},
	{serviceErrors | agentErrors, matches(auth.ErrRateLimited), http.StatusTooManyRequests, "rate_limited", "Request rate limit reached; retry later", true},
	{serviceErrors | agentErrors, matches(nodes.ErrDisabled), http.StatusForbidden, "node_disabled", "The node is disabled", false},
	{serviceErrors | agentErrors, matches(nodes.ErrCapacityReached), http.StatusConflict, "node_capacity_reached", "Configured node capacity has been reached", false},
	{serviceErrors | agentErrors, matches(nodes.ErrNotFound, traffic.ErrNotFound), http.StatusNotFound, "node_not_found", "Node not found", false},
	{serviceErrors | agentErrors, matches(notify.ErrCapacity), http.StatusConflict, "notification_channel_capacity_reached", "Maximum of 20 notification channels reached", false},
	{serviceErrors | agentErrors, matches(notify.ErrSecretUnreadable), http.StatusConflict, "secret_unreadable", "Restore the notification encryption key or replace the affected secret and webhook URL", false},
	{serviceErrors | agentErrors, matches(notify.ErrUnavailable), http.StatusServiceUnavailable, "not_available", "Notification secrets unavailable", false},
	{serviceErrors | agentErrors, matches(alerts.ErrCapacity), http.StatusConflict, "alert_rule_capacity_reached", "Maximum of 200 alert rules reached", false},
	{serviceErrors | agentErrors, matches(notify.ErrNotFound, alerts.ErrNotFound, auth.ErrNotFound, management.ErrNotFound, inbox.ErrNotFound), http.StatusNotFound, "not_found", "Resource not found", false},
	{serviceErrors | agentErrors, matches(auth.ErrLastAdmin), http.StatusConflict, "last_administrator", "At least one administrator must remain", false},
	{serviceErrors | agentErrors, matches(auth.ErrUsernameConflict), http.StatusConflict, "username_conflict", "Username is already in use", false},
	{serviceErrors | agentErrors, matches(nodes.ErrInvalidInput, auth.ErrInvalidInput, management.ErrInvalidInput, inbox.ErrInvalidInput, traffic.ErrInvalidInput), http.StatusBadRequest, "validation_failed", "Request values are invalid", false},
	{backupErrors, backupCodes("invalid_input", "invalid_actor"), http.StatusBadRequest, "", "The request contains invalid parameters", false},
	{backupErrors, backupCodes("forbidden"), http.StatusForbidden, "", "Administrator access is required", false},
	{backupErrors, backupCodes("backup_queue_full"), http.StatusTooManyRequests, "", "The backup queue is full; try again after a task finishes", false},
	{backupErrors, backupCodes("backup_plan_busy"), http.StatusConflict, "", "This plan has a queued or running backup; try again after it finishes", false},
	{backupErrors, backupCodes("backup_plan_not_found", "task_not_found", "password_unavailable"), http.StatusNotFound, "", "The requested backup resource is unavailable", false},
	{backupErrors, backupCodes("artifact_not_ready"), http.StatusConflict, "", "The download is not ready", false},
	{backupErrors, backupCodes("artifact_expired", "artifact_missing", "password_expired"), http.StatusGone, "", "The requested backup artifact or password is no longer available", false},
	{backupErrors, backupCodes("secret_unreadable"), http.StatusConflict, "", "The backup secret cannot be read with the server key", false},
}

func matches(targets ...error) func(error) bool {
	return func(err error) bool {
		for _, target := range targets {
			if errors.Is(err, target) {
				return true
			}
		}
		return false
	}
}

func backupCodes(codes ...string) func(error) bool {
	return func(err error) bool {
		var coded *backup.Error
		if !errors.As(err, &coded) {
			return false
		}
		for _, code := range codes {
			if coded.Code == code {
				return true
			}
		}
		return false
	}
}

func isFieldValidation(err error) bool {
	var fields *validation.Error
	return errors.As(err, &fields)
}

func isBodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

func mappedError(w http.ResponseWriter, err error, requestID string, scope errorScope) bool {
	for _, mapping := range errorMappings {
		if mapping.scopes&scope == 0 || !mapping.match(err) {
			continue
		}
		response := apiError{Code: mapping.code, Message: mapping.message, RequestID: requestID}
		if response.Message == "" {
			var fields *validation.Error
			errors.As(err, &fields)
			response.Message, response.Fields = fields.Error(), fields.Fields
		}
		if response.Code == "" {
			var coded *backup.Error
			errors.As(err, &coded)
			response.Code = coded.Code
		}
		if mapping.retry {
			w.Header().Set("Retry-After", "1")
		}
		writeJSON(w, mapping.status, response)
		return true
	}
	return false
}

func (h *handler) writeError(w http.ResponseWriter, err error, requestID string, scope errorScope, fallback string) {
	if mappedError(w, err, requestID, scope) {
		return
	}
	h.logInternalError(err, requestID)
	fail(w, http.StatusInternalServerError, "internal_error", fallback, requestID)
}

func (h *handler) serviceError(w http.ResponseWriter, err error, requestID string) {
	h.writeError(w, err, requestID, serviceErrors, "The operation could not be completed")
}

func (h *handler) agentError(w http.ResponseWriter, err error, requestID string) {
	h.writeError(w, err, requestID, agentErrors, "The operation could not be completed")
}

func (h *handler) backupError(w http.ResponseWriter, err error, requestID string) {
	h.writeError(w, err, requestID, backupErrors, "The backup operation could not be completed")
}

func decodeError(w http.ResponseWriter, err error, message, requestID string) {
	if !mappedError(w, err, requestID, decodeErrors) {
		fail(w, http.StatusBadRequest, "invalid_json", message, requestID)
	}
}
