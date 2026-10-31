package server

import (
	"bufio"
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

type apiError struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
	Fields    map[string]string `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body, _ = json.Marshal(apiError{Code: "internal_error", Message: "The operation could not be completed", RequestID: w.Header().Get("X-Request-ID")})
	}
	writeJSONBody(w, status, body)
}

func writeJSONBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func fail(w http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(w, status, apiError{Code: code, Message: message, RequestID: requestID})
}

func decode(w http.ResponseWriter, r *http.Request, dst any, requestID string) bool {
	return decodeJSON(w, r, dst, requestID, true)
}

func decodeTelemetry(w http.ResponseWriter, r *http.Request, dst any, requestID string) bool {
	return decodeJSON(w, r, dst, requestID, false)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, requestID string, strict bool) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		fail(w, http.StatusBadRequest, "invalid_json", "Content-Type must be application/json", requestID)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	reader := bufio.NewReader(r.Body)
	for {
		first, err := reader.ReadByte()
		if err != nil {
			decodeError(w, err, "Request must contain a JSON object", requestID)
			return false
		}
		if first == ' ' || first == '\t' || first == '\r' || first == '\n' {
			continue
		}
		if first != '{' {
			fail(w, http.StatusBadRequest, "invalid_json", "Request must contain a JSON object", requestID)
			return false
		}
		if err := reader.UnreadByte(); err != nil {
			fail(w, http.StatusBadRequest, "invalid_json", "Unable to read JSON object", requestID)
			return false
		}
		break
	}
	decoder := json.NewDecoder(reader)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(dst); err != nil {
		decodeError(w, err, "Request must contain a valid JSON object", requestID)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		decodeError(w, err, "Request must contain exactly one JSON value", requestID)
		return false
	}
	return true
}
