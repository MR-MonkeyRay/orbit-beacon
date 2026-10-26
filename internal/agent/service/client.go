package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

type ClientOptions struct {
	ServerURL  string
	CAFile     string
	HTTPClient *http.Client
	Identity   *identity.Store
	Config     *ConfigStore
	System     v1.System
	// AllowInsecureLoopback is for explicit local integration tests only.
	AllowInsecureLoopback bool
}

type Client struct {
	baseURL  string
	http     *http.Client
	identity *identity.Store
	config   *ConfigStore
	system   v1.System
}

// HTTPError never includes URLs, bearer values or server-controlled response text.
type HTTPError struct {
	StatusCode int
	RetryAfter string
	Code       string
}

func (e *HTTPError) Error() string {
	if e.StatusCode == http.StatusUnauthorized {
		return "credentials_invalid"
	}
	return "agent request rejected by server"
}

func NewClient(options ClientOptions) (*Client, error) {
	if options.Identity == nil || options.Config == nil {
		return nil, errors.New("agent identity and config are required")
	}
	parsed, parseErr := url.Parse(options.ServerURL)
	if parseErr != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
		return nil, errors.New("server URL must use the root path")
	}
	origin, err := config.CanonicalOrigin(options.ServerURL)
	if err != nil {
		parsed, parseErr := url.Parse(options.ServerURL)
		if !options.AllowInsecureLoopback || parseErr != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(options.ServerURL, "#") || parsed.Opaque != "" {
			return nil, errors.New("server URL must be HTTPS without credentials, query or fragment")
		}
		address, err := netip.ParseAddr(parsed.Hostname())
		if err != nil || !address.IsLoopback() {
			return nil, errors.New("test HTTP requires a literal loopback address")
		}
		origin = "http://" + parsed.Host
	}
	client := options.HTTPClient
	if client == nil {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if options.CAFile != "" {
			file, err := openCABundle(options.CAFile, 0)
			if err != nil {
				return nil, err
			}
			pem, readErr := io.ReadAll(io.LimitReader(file, maxCABundleSize+1))
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || len(pem) > maxCABundleSize {
				return nil, errors.New("read configured CA file failed or exceeded 1 MiB")
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("configured CA file contains no certificates")
			}
		}
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			MaxIdleConns:          2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2,
			IdleConnTimeout: 90 * time.Second,
		}}
	}
	// Preserve the caller's trusted transport, but never redirect a bearer request.
	copy := *client
	copy.Timeout = 5 * time.Second
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: origin, http: &copy, identity: options.Identity, config: options.Config, system: options.System}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Enroll(ctx context.Context, token string) (v1.EnrollmentResponse, error) {
	state := c.identity.Current()
	validTicket := token != "" && !strings.ContainsAny(token, "\r\n")
	if !validTicket || (state.RunGeneration > 0 && state.NodeID == "" && state.PendingNodeToken == "") {
		response, err := c.recoverEnrollment(ctx, state)
		if err == nil {
			return response, nil
		}
		var rejected *HTTPError
		if !errors.As(err, &rejected) || rejected.StatusCode != http.StatusUnauthorized {
			return v1.EnrollmentResponse{}, err
		}
		if !validTicket {
			return v1.EnrollmentResponse{}, errors.New("enrollment ticket is required; saved node credentials were not accepted")
		}
	}
	if state.NodeID != "" || state.PendingNodeToken != "" {
		var err error
		state, err = c.identity.StageRotation()
		if err != nil {
			return v1.EnrollmentResponse{}, err
		}
	}
	credential := state.NodeToken
	if state.PendingNodeToken != "" {
		credential = state.PendingNodeToken
	}
	request := v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: state.InstallationID, NodeToken: credential, System: c.system}
	var response v1.EnrollmentResponse
	if err := c.post(ctx, "/api/agent/v1/enroll", token, request, &response); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if response.NodeID == "" {
		return v1.EnrollmentResponse{}, errors.New("invalid enrollment response")
	}
	if err := c.identity.ConfirmEnrollment(credential, response.NodeID); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	return response, nil
}

func (c *Client) recoverEnrollment(ctx context.Context, state identity.Identity) (v1.EnrollmentResponse, error) {
	credential := state.NodeToken
	if state.PendingNodeToken != "" {
		credential = state.PendingNodeToken
	}
	request := v1.RecoveryRequest{InstallationID: state.InstallationID}
	var response v1.EnrollmentResponse
	err := c.post(ctx, "/api/agent/v1/recover", credential, request, &response)
	var rejected *HTTPError
	if err != nil && credential != state.NodeToken && errors.As(err, &rejected) && rejected.StatusCode == http.StatusUnauthorized {
		credential = state.NodeToken
		err = c.post(ctx, "/api/agent/v1/recover", credential, request, &response)
	}
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if response.NodeID == "" || (state.NodeID != "" && response.NodeID != state.NodeID) {
		return v1.EnrollmentResponse{}, errors.New("invalid enrollment recovery response")
	}
	if err := c.identity.ConfirmEnrollment(credential, response.NodeID); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	return response, nil
}

// Report authenticates using the durable node secret even after a lost enrollment response.
func (c *Client) Report(ctx context.Context, report v1.Report) (v1.ReportResponse, error) {
	var response v1.ReportResponse
	if report.ConfigRevision == 0 {
		report.ConfigRevision, _ = c.config.settings()
	}
	report.ConfigError = c.config.ConfigError()
	state := c.identity.Current()
	credential := state.NodeToken
	err := c.post(ctx, "/api/agent/v1/report", credential, report, &response)
	var rejected *HTTPError
	if err != nil && errors.As(err, &rejected) && rejected.StatusCode == http.StatusUnauthorized {
		current, reloadErr := c.identity.Reload()
		if reloadErr != nil {
			return v1.ReportResponse{}, reloadErr
		}
		candidate := current.NodeToken
		if candidate == credential {
			candidate = current.PendingNodeToken
		}
		if candidate != "" && candidate != credential {
			response = v1.ReportResponse{}
			err = c.post(ctx, "/api/agent/v1/report", candidate, report, &response)
			credential = candidate
			state = current
		}
	}
	if err != nil {
		return response, err
	}
	if state.NodeID == "" || credential != state.NodeToken || response.NodeID != state.NodeID {
		// Avoid steady-state writes; bind first-report recovery and validate any
		// changed credential/node pair against the latest durable identity.
		if err := c.identity.ConfirmEnrollment(credential, response.NodeID); err != nil {
			return v1.ReportResponse{}, err
		}
	}
	if response.DesiredConfig != nil {
		// The server accepted the sample. A rejected setting is reported on the next
		// sample, without converting acceptance into a network retry.
		_ = c.config.Apply(*response.DesiredConfig)
	}
	return response, nil
}

func (c *Client) post(ctx context.Context, path, token string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("cannot encode agent request")
	}
	if len(data) > 64*1024 {
		return errors.New("agent request exceeds 64 KiB")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("cannot construct agent request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("agent connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code string `json:"code"`
		}
		// Error text is never propagated; only the fixed protocol code is used
		// to distinguish permanent ownership loss from transient rejection.
		_ = json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&failure)
		code := ""
		if failure.Code == "identity_conflict" {
			code = failure.Code
		}
		return &HTTPError{StatusCode: response.StatusCode, RetryAfter: response.Header.Get("Retry-After"), Code: code}
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return errors.New("invalid agent response size")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("invalid agent response JSON")
	}
	return nil
}
