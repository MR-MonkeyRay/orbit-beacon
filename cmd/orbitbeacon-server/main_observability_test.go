package main

import (
	"net"
	"path/filepath"
	"testing"
)

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
