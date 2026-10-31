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
		return errors.New("health 需要 --url")
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
		return errors.New("health CA 仅适用于 HTTPS URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("读取 health CA 失败: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return errors.New("health CA 中没有任何证书")
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
		return fmt.Errorf("health 请求失败: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器未就绪: %s", response.Status)
	}
	fmt.Fprintln(os.Stdout, "ready")
	return nil
}
