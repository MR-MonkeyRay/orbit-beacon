package server

import (
	"net/http"
	"orbitbeacon/internal/server/auth"
	"strings"
)

func (h *handler) accountTOTP(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session, token string) {
	if r.Method == http.MethodGet {
		status, err := h.auth.TOTPStatus(r.Context(), session.User.ID)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, status)
		return
	}
	var output any
	var err error
	switch strings.TrimPrefix(r.URL.Path, "/api/v1/account/totp/") {
	case "setup":
		var input struct {
			Password string `json:"password"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		output, err = h.auth.SetupTOTP(r.Context(), session.User.ID, input.Password)
	case "confirm":
		var input struct {
			Code string `json:"code"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		var codes []string
		codes, err = h.auth.ConfirmTOTP(r.Context(), session.User.ID, token, input.Code)
		output = map[string]any{"recovery_codes": codes}
	case "disable", "recovery-codes":
		var input struct {
			Password string `json:"password"`
			OTP      string `json:"otp"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/disable") {
			err = h.auth.DisableTOTP(r.Context(), session.User.ID, token, input.Password, input.OTP)
		} else {
			var codes []string
			codes, err = h.auth.RegenerateRecoveryCodes(r.Context(), session.User.ID, token, input.Password, input.OTP)
			output = map[string]any{"recovery_codes": codes}
		}
	}
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	if output == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, output)
}
func (h *handler) resetUserTOTP(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session, token string) {
	var input struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &input, requestID) {
		return
	}
	id := strings.Split(r.URL.Path, "/")[4]
	if err := h.auth.ResetUserTOTP(r.Context(), session.User.ID, token, id, input.Password); err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
