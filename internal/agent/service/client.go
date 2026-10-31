package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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
	System     v1.System
}

type Client struct {
	baseURL  string
	http     *http.Client
	identity *identity.Store
	system   v1.System
}

// HTTPError 绝不包含 URL、Bearer 凭据或服务器可控的响应文本。
type HTTPError struct {
	StatusCode int
	RetryAfter string
	Code       string
}

func (e *HTTPError) Error() string {
	if e.StatusCode == http.StatusUnauthorized {
		return "credentials_invalid"
	}
	if e.StatusCode == http.StatusConflict && e.Code == "identity_conflict" {
		return "identity_conflict"
	}
	return "agent request rejected by server"
}

var (
	errAgentConnectionFailed = errors.New("agent connection failed")
	errAgentTimeout          = errors.New("agent request timed out")
)

func NewClient(options ClientOptions) (*Client, error) {
	if options.Identity == nil {
		return nil, errors.New("agent identity is required")
	}
	if err := config.ValidateServerURL(options.ServerURL); err != nil {
		return nil, err
	}
	origin, err := config.CanonicalOrigin(options.ServerURL)
	if err != nil {
		return nil, err
	}
	secure := strings.HasPrefix(origin, "https://")
	if !secure && options.CAFile != "" {
		return nil, errors.New("configured CA file requires an HTTPS server URL")
	}
	client := options.HTTPClient
	if client == nil {
		var tlsConfig *tls.Config
		if secure {
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
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		}
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			MaxIdleConns:          2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2,
			IdleConnTimeout: 90 * time.Second,
		}}
	}
	// 保留调用方可信的传输实现，但禁止携带 Bearer 凭据的请求重定向。
	copy := *client
	copy.Timeout = 5 * time.Second
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: origin, http: &copy, identity: options.Identity, system: options.System}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Enroll(ctx context.Context, token string) (v1.EnrollmentResponse, error) {
	state, err := c.identity.Reload(ctx)
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if strings.ContainsAny(token, "\r\n") {
		return v1.EnrollmentResponse{}, errors.New("invalid explicit enrollment ticket")
	}
	if token == "" {
		response, err := c.recoverEnrollment(ctx, state)
		if err == nil {
			return response, nil
		}
		var rejected *HTTPError
		if !errors.As(err, &rejected) || rejected.StatusCode != http.StatusUnauthorized {
			return v1.EnrollmentResponse{}, err
		}
		return v1.EnrollmentResponse{}, errors.New("enrollment ticket is required; saved node credentials were not accepted")
	}
	ticket := sha256.Sum256([]byte(token))
	state, operation, exists, err := c.identity.LookupEnrollment(ctx, ticket)
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if !exists {
		// 旧身份可能在此票据的响应到达前，就已通过上报提升了已提交的候选项。
		// 只检查其回执，不消费新票据，也不暂存另一个候选项。
		credential := state.NodeToken
		if state.PendingNodeToken != "" {
			credential = state.PendingNodeToken
		}
		var checked v1.EnrollmentCheckResponse
		check := v1.EnrollmentCheckRequest{ProtocolVersion: v1.EnrollmentCheckVersion, InstallationID: state.InstallationID, NodeToken: credential}
		err := c.post(ctx, "/api/agent/v1/enroll/check", token, check, &checked)
		var conflict *HTTPError
		if credential != state.NodeToken && errors.As(err, &conflict) && conflict.StatusCode == http.StatusConflict {
			// 另一个未提交的待定候选项不得遮蔽遗留文件中的有效回执。
			// 两次读取都没有副作用。
			credential = state.NodeToken
			check.NodeToken = credential
			checked = v1.EnrollmentCheckResponse{}
			err = c.post(ctx, "/api/agent/v1/enroll/check", token, check, &checked)
		}
		if err != nil {
			return v1.EnrollmentResponse{}, err
		}
		if checked.ServerTime.IsZero() || (checked.CredentialState != "active" && checked.CredentialState != "unbound") || (checked.Claimed && (checked.NodeID == "" || checked.CredentialState != "active")) || (!checked.Claimed && checked.NodeID != "") {
			return v1.EnrollmentResponse{}, errors.New("invalid enrollment receipt response")
		}
		if checked.Claimed {
			if state.NodeID != "" && state.NodeID != checked.NodeID {
				return v1.EnrollmentResponse{}, errors.New("enrollment receipt does not match durable node")
			}
			if _, _, err := c.identity.RememberEnrollment(ctx, ticket, credential); err != nil {
				return v1.EnrollmentResponse{}, err
			}
			if err := c.identity.ConfirmEnrollment(ctx, credential, checked.NodeID); err != nil {
				return v1.EnrollmentResponse{}, err
			}
			return v1.EnrollmentResponse{NodeID: checked.NodeID, ServerTime: checked.ServerTime}, nil
		}
		state, operation, err = c.identity.StageEnrollment(ctx, ticket, state.NodeID != "" || checked.CredentialState == "active")
		if err != nil {
			return v1.EnrollmentResponse{}, err
		}
	}
	request := v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: state.InstallationID, NodeToken: operation.NodeToken, PreviousNodeToken: operation.PreviousNodeToken, System: c.system}
	var response v1.EnrollmentResponse
	if err := c.post(ctx, "/api/agent/v1/enroll", token, request, &response); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if response.NodeID == "" {
		return v1.EnrollmentResponse{}, errors.New("invalid enrollment response")
	}
	if err := c.identity.ConfirmEnrollment(ctx, operation.NodeToken, response.NodeID); err != nil {
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
	if err := c.identity.ConfirmEnrollment(ctx, credential, response.NodeID); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	return response, nil
}

// Report 使用持久节点密钥认证，即使注册响应丢失也不受影响。
func (c *Client) Report(ctx context.Context, report v1.Report) (v1.ReportResponse, error) {
	var response v1.ReportResponse
	state := c.identity.Current()
	credential := state.NodeToken
	err := c.post(ctx, "/api/agent/v1/report", credential, report, &response)
	var rejected *HTTPError
	if err != nil && errors.As(err, &rejected) && rejected.StatusCode == http.StatusUnauthorized {
		current, reloadErr := c.identity.Reload(ctx)
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
		// 正常运行时避免重复写入；首次上报恢复时补全节点绑定，
		// 并将变化后的凭据/节点组合与最新的持久化身份核对。
		if err := c.identity.ConfirmEnrollment(ctx, credential, response.NodeID); err != nil {
			return v1.ReportResponse{}, err
		}
	}
	return response, nil
}

func (c *Client) post(ctx context.Context, path, token string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("cannot encode agent request")
	}
	if len(data) > v1.MaxMessageBytes {
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
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			return errAgentTimeout
		}
		return errAgentConnectionFailed
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code string `json:"code"`
		}
		// 错误文本绝不向外传播；只用固定的协议错误码区分
		// 永久性的所有权丢失与临时拒绝。
		_ = json.NewDecoder(io.LimitReader(response.Body, v1.MaxMessageBytes)).Decode(&failure)
		code := ""
		if failure.Code == "identity_conflict" {
			code = failure.Code
		}
		return &HTTPError{StatusCode: response.StatusCode, RetryAfter: response.Header.Get("Retry-After"), Code: code}
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, v1.MaxMessageBytes+1))
	if err != nil || len(data) > v1.MaxMessageBytes {
		return errors.New("invalid agent response size")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("invalid agent response JSON")
	}
	return nil
}
