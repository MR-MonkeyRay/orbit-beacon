package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"orbitbeacon/internal/config"
)

func checkHealth(rawURL, caFile string) error {
	if rawURL == "" {
		return errors.New("health requires --url")
	}
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("health URL: %w", err)
	}
	if err := config.ValidateServerURL(rawURL); err != nil {
		return fmt.Errorf("health URL: %w", err)
	}
	request.URL.Path = "/healthz/ready"
	if request.URL.Scheme == "http" && caFile != "" {
		return errors.New("health CA requires an HTTPS URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("read health CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return errors.New("health CA contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{
		Timeout:       3 * time.Second,
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("server is not ready: %s", response.Status)
	}
	fmt.Fprintln(os.Stdout, "ready")
	return nil
}
