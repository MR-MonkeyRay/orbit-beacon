package main

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
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
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "transport failure") {
		t.Fatalf("bridged log = %q, want warn-level message", line)
	}
}

func TestOccupiedMetricsListenerStopsMainListener(t *testing.T) {
	mainAddress := unusedAddress(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	err = run([]string{
		"serve",
		"--data-dir", filepath.Join(t.TempDir(), "data"),
		"--public-url", "https://beacon.example.test",
		"--listen-addr", mainAddress,
		"--metrics-listen", occupied.Addr().String(),
	})
	if err == nil {
		t.Fatal("serve succeeded with an occupied metrics listener")
	}

	listener, err := net.Listen("tcp", mainAddress)
	if err != nil {
		t.Fatalf("main listener was not released after metrics startup failure: %v", err)
	}
	listener.Close()
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
