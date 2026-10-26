package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

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
