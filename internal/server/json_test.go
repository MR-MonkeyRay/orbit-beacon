package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSONEncodingFailureReturnsJSONError(t *testing.T) {
	for _, value := range []any{math.Inf(1), math.NaN(), make(chan int)} {
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-ID", "encoding-request")
		writeJSON(w, http.StatusOK, value)
		var result apiError
		if w.Code != http.StatusInternalServerError || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Code != "internal_error" || result.RequestID != "encoding-request" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		if w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("headers=%v", w.Header())
		}
	}
}
