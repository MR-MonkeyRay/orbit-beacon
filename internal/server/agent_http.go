package server

import (
	"errors"
	"net/http"
	"strings"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
)

func bearer(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func (h *handler) agentRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	token := bearer(r)
	if r.URL.Path == "/api/agent/v1/enroll" || r.URL.Path == "/api/agent/v1/enroll/check" {
		if err := h.enrollments.AdmitToken(r.Context(), token, h.clientAddr(r)); err != nil {
			h.agentError(w, err, requestID)
			return
		}
		if r.URL.Path == "/api/agent/v1/enroll/check" {
			var input v1.EnrollmentCheckRequest
			if !decode(w, r, &input, requestID) {
				return
			}
			result, err := h.enrollments.CheckClaim(r.Context(), token, input)
			if err != nil {
				h.agentError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		var input v1.EnrollmentRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		if input.ProtocolVersion != 1 {
			h.agentError(w, ingest.ErrProtocolUnsupported, requestID)
			return
		}
		result, err := h.enrollments.Enroll(r.Context(), token, input)
		if err != nil {
			h.agentError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if token == "" {
		fail(w, http.StatusUnauthorized, "unauthenticated", "A valid agent credential is required", requestID)
		return
	}
	if r.URL.Path == "/api/agent/v1/recover" {
		// Reject unknown credentials before reading a recovery request, without
		// charging report admission or taking the ingest writer.
		if _, err := h.nodes.AuthenticateToken(r.Context(), token); err != nil {
			h.agentError(w, err, requestID)
			return
		}
		var input v1.RecoveryRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		result, err := h.enrollments.Recover(r.Context(), token, input)
		if err != nil {
			h.agentError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	// Authentication and admission happen before any request body is read.
	admission, err := h.ingest.AdmitToken(r.Context(), token)
	if err != nil {
		h.agentError(w, err, requestID)
		return
	}
	var input v1.Report
	if !decodeTelemetry(w, r, &input, requestID) {
		return
	}
	result, err := h.ingest.AcceptAdmitted(r.Context(), admission, input)
	if err != nil {
		h.agentError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) agentError(w http.ResponseWriter, err error, requestID string) {
	switch {
	case errors.Is(err, ingest.ErrProtocolUnsupported), errors.Is(err, v1.ErrProtocolUnsupported):
		fail(w, http.StatusUpgradeRequired, "protocol_unsupported", "Protocol version is not supported", requestID)
	case errors.Is(err, enrollment.ErrConflict):
		fail(w, http.StatusConflict, "enrollment_conflict", "Enrollment does not match the bound installation and credential", requestID)
	case errors.Is(err, enrollment.ErrExpired), errors.Is(err, enrollment.ErrRevoked), errors.Is(err, enrollment.ErrUnauthenticated):
		fail(w, http.StatusUnauthorized, "unauthenticated", "Enrollment credential is invalid, expired or revoked", requestID)
	case errors.Is(err, enrollment.ErrNotFound):
		fail(w, http.StatusNotFound, "enrollment_not_found", "Enrollment not found", requestID)
	case errors.Is(err, enrollment.ErrInvalidInput), errors.Is(err, ingest.ErrInvalidReport):
		fail(w, http.StatusBadRequest, "validation_failed", "Request values are invalid", requestID)
	case errors.Is(err, ingest.ErrIdentityConflict):
		fail(w, http.StatusConflict, "identity_conflict", "A newer generation or another session owns this identity", requestID)
	case errors.Is(err, enrollment.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusTooManyRequests, "rate_limited", "Enrollment rate limit exceeded", requestID)
	case errors.Is(err, ingest.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusTooManyRequests, "rate_limited", "Report rate limit exceeded", requestID)
	case errors.Is(err, ingest.ErrWriterBusy):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusServiceUnavailable, "report_busy", "Report writer is busy", requestID)
	case errors.Is(err, ingest.ErrServiceClosed):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusServiceUnavailable, "service_unavailable", "Report ingestion is shutting down", requestID)
	default:
		h.serviceError(w, err, requestID)
	}
}

func (h *handler) enrollmentRoute(w http.ResponseWriter, r *http.Request, requestID string) bool {
	if r.URL.Path != "/api/v1/enrollments" && !strings.HasPrefix(r.URL.Path, "/api/v1/enrollments/") {
		return false
	}
	if r.URL.Path != "/api/v1/enrollments" {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/enrollments/")
		if r.Method == http.MethodDelete {
			if err := h.enrollments.Revoke(r.Context(), id); err != nil {
				h.agentError(w, err, requestID)
				return true
			}
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		result, err := h.enrollments.Get(r.Context(), id)
		if err != nil {
			h.agentError(w, err, requestID)
			return true
		}
		writeJSON(w, http.StatusOK, result)
		return true
	}
	if r.Method == http.MethodGet {
		limit, offset, err := pagination(r)
		if err != nil {
			h.agentError(w, enrollment.ErrInvalidInput, requestID)
			return true
		}
		result, err := h.enrollments.List(r.Context(), limit+1, offset)
		if err != nil {
			h.agentError(w, err, requestID)
			return true
		}
		hasMore := len(result) > limit
		response := map[string]any{"has_more": hasMore}
		if hasMore {
			result = result[:limit]
			response["next_offset"] = offset + limit
		}
		response["enrollments"] = result
		writeJSON(w, http.StatusOK, response)
		return true
	}
	var input struct {
		NodeID  string `json:"node_id"`
		GroupID string `json:"group_id"`
		Mode    string `json:"mode"`
	}
	if !decode(w, r, &input, requestID) {
		return true
	}
	var result enrollment.CreatedTicket
	var err error
	switch {
	case input.NodeID != "" && input.GroupID == "" && (input.Mode == "" || input.Mode == "single"):
		result, err = h.enrollments.CreateSingle(r.Context(), input.NodeID)
	case input.NodeID == "" && (input.Mode == "batch" || input.GroupID != "") && (input.Mode == "" || input.Mode == "batch"):
		result, err = h.enrollments.CreateBatch(r.Context(), input.GroupID)
	default:
		err = enrollment.ErrInvalidInput
	}
	if err != nil {
		h.agentError(w, err, requestID)
		return true
	}
	writeJSON(w, http.StatusCreated, result)
	return true
}
