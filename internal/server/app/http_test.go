package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPServerBridgesErrorLogToSlogAtWarnLevel(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
	got := newHTTPServer("127.0.0.1:0", http.NotFoundHandler(), logger)

	if got.ErrorLog == nil {
		t.Fatal("http.Server.ErrorLog is nil")
	}
	got.ErrorLog.Print("transport failure")
	line := output.String()
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "http_transport_error") || strings.Contains(line, "transport failure") {
		t.Fatalf("bridged log = %q, want warn-level message", line)
	}
}
