package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckHealthLoopbackHTTP(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(address, func(t *testing.T) {
			listener, err := net.Listen("tcp", address)
			if err != nil {
				if strings.HasPrefix(address, "[::1]") {
					t.Skipf("IPv6 loopback unavailable: %v", err)
				}
				t.Fatal(err)
			}
			var status atomic.Int32
			status.Store(http.StatusOK)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/healthz/ready" || r.URL.RawQuery != "" {
					t.Errorf("unexpected health request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Location", "/healthz/live")
				w.WriteHeader(int(status.Load()))
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			if err := run([]string{"health", "--url", server.URL + "/"}); err != nil {
				t.Fatalf("ready endpoint failed: %v", err)
			}
			for _, code := range []int{http.StatusServiceUnavailable, http.StatusFound} {
				status.Store(int32(code))
				if err := checkHealth(server.URL, ""); err == nil {
					t.Errorf("health accepted status %d", code)
				}
			}
		})
	}
}

func TestCheckHealthRejectsUnsafeOrigins(t *testing.T) {
	for _, origin := range []string{
		"",
		"http://127.0.0.1@192.0.2.1", "http://user@127.0.0.1",
		"http://127.0.0.1/prefix", "http://127.0.0.1/%2f",
		"http://127.0.0.1?x=1", "http://127.0.0.1?", "http://127.0.0.1#",
		"http://[::1%25lo]", "ftp://127.0.0.1", "://bad-url",
	} {
		t.Run(origin, func(t *testing.T) {
			if err := checkHealth(origin, ""); err == nil || (origin != "" && !strings.Contains(err.Error(), "health URL:")) {
				t.Fatalf("expected URL rejection before connecting, got %v", err)
			}
		})
	}
}

func TestCheckHealthHTTPHostnameAndCAConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz/ready" {
			t.Errorf("unexpected health path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	origin := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	if err := checkHealth(origin, ""); err != nil {
		t.Fatal(err)
	}
	if err := checkHealth(origin, filepath.Join(t.TempDir(), "missing-ca.pem")); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("HTTP with CA must reject encryption mismatch before reading CA: %v", err)
	}
}

func TestCheckHealthHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	started := time.Now()
	err := checkHealth(server.URL, "")
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("expected a health request timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("health exceeded the container timeout: %s", elapsed)
	}
}

func TestCheckHealthUsesPinnedHTTPSCAAndReadinessStatus(t *testing.T) {
	ready := true
	var targetHits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz/ready" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		targetHits.Add(1)
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkHealth(server.URL, caPath); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	wrongCAPath := filepath.Join(t.TempDir(), "wrong-ca.pem")
	if err := os.WriteFile(wrongCAPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkHealth(server.URL, wrongCAPath); err == nil {
		t.Fatal("health accepted a CA bundle that does not trust the server")
	}
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/healthz/ready", http.StatusFound)
	}))
	defer redirect.Close()
	redirectCAPath := filepath.Join(t.TempDir(), "redirect-ca.pem")
	if err := os.WriteFile(redirectCAPath, append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})...), 0600); err != nil {
		t.Fatal(err)
	}
	hitsBefore := targetHits.Load()
	if err := checkHealth(redirect.URL, redirectCAPath); err == nil {
		t.Fatal("health followed a redirect")
	}
	if targetHits.Load() != hitsBefore {
		t.Fatal("health request reached the redirected target")
	}
	ready = false
	if err := checkHealth(server.URL, caPath); err == nil {
		t.Fatal("not-ready endpoint was accepted")
	}
}
