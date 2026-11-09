package storagetest

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"

	"orbitbeacon/internal/config"
)

func fixtureTLS(endpoint config.DatabaseEndpoint) (*tls.Config, error) {
	if endpoint.Network == "unix" || endpoint.TLS == nil {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if endpoint.TLS != nil {
		if endpoint.TLS.ServerName != "" {
			cfg.ServerName = endpoint.TLS.ServerName
		}
		if endpoint.TLS.CAFile != "" {
			data, err := os.ReadFile(endpoint.TLS.CAFile)
			if err != nil {
				return nil, err
			}
			cfg.RootCAs = x509.NewCertPool()
			if !cfg.RootCAs.AppendCertsFromPEM(data) {
				return nil, errors.New("invalid fixture CA")
			}
		}
	}
	return cfg, nil
}
