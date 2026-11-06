package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"orbitbeacon/internal/server/auth"
	"testing"
)

func TestTOTPRejectionLogging(t *testing.T) {
	var logs bytes.Buffer
	h := &handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	r := httptest.NewRequest("POST", "/api/v1/session", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	h.logLoginRejection(auth.ErrTOTPRequired, r, "request")
	if logs.Len() != 0 {
		t.Fatal("required was logged")
	}
	h.logLoginRejection(auth.ErrTOTPInvalid, r, "request")
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["reason"] != "invalid_second_factor" {
		t.Fatal(entry)
	}
	for key := range entry {
		switch key {
		case "time", "level", "msg", "request_id", "peer", "reason":
		default:
			t.Errorf("unexpected log member %s", key)
		}
	}
}
