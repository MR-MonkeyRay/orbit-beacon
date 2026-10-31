package server

import (
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
		// 在读取恢复请求前拒绝未知凭据，不消耗上报限流额度，也不占用 ingest 写入槽。
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
	// 认证和准入在读取任何请求体之前完成。
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

func (h *handler) enrollmentRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.URL.Path != "/api/v1/enrollments" {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/enrollments/")
		if r.Method == http.MethodDelete {
			if err := h.enrollments.Revoke(r.Context(), id); err != nil {
				h.agentError(w, err, requestID)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		result, err := h.enrollments.Get(r.Context(), id)
		if err != nil {
			h.agentError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if r.Method == http.MethodGet {
		limit, offset, err := pagination(r)
		if err != nil {
			h.agentError(w, enrollment.ErrInvalidInput, requestID)
			return
		}
		result, err := h.enrollments.List(r.Context(), limit+1, offset)
		if err != nil {
			h.agentError(w, err, requestID)
			return
		}
		hasMore := len(result) > limit
		response := map[string]any{"has_more": hasMore}
		if hasMore {
			result = result[:limit]
			response["next_offset"] = offset + limit
		}
		response["enrollments"] = result
		writeJSON(w, http.StatusOK, response)
		return
	}
	var input struct {
		NodeID  string `json:"node_id"`
		GroupID string `json:"group_id"`
		Mode    string `json:"mode"`
	}
	if !decode(w, r, &input, requestID) {
		return
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
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
