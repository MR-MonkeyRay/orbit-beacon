package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"orbitbeacon/internal/buildinfo"
	v1 "orbitbeacon/internal/protocol/v1"
)

const (
	defaultTimeout = 5 * time.Second
	maxHTTPBody    = 64 << 10
)

// Checker 执行单个服务探测，可并发使用。
type Checker struct {
	lookupIPAddr func(context.Context, string) ([]net.IPAddr, error)
	timeout      time.Duration
	transport    *http.Transport
	client       *http.Client
}

// NewChecker 创建探测器，使用给定的单次探测超时。
// 超时为零或负数时默认使用五秒。
func NewChecker(timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	// 每次探测都重新建立连接，将连接建立和 TLS 握手计入延迟及失败，
	// 避免空闲连接跨越 60 秒的探测间隔。
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Checker{lookupIPAddr: net.DefaultResolver.LookupIPAddr, timeout: timeout, transport: transport, client: client}
}

// Close 释放探测器持有的空闲 HTTP 连接。
func (c *Checker) Close() {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// Check 对目标探测一次，返回可直接上报的结果。
func (c *Checker) Check(ctx context.Context, target string) v1.ProbeResult {
	return c.CheckExpectedStatus(ctx, target, 0)
}

// CheckExpectedStatus 使用本地期望状态；零表示接受所有低于 400 的响应。
func (c *Checker) CheckExpectedStatus(ctx context.Context, target string, expected int) v1.ProbeResult {
	checkedAt := time.Now().UTC()
	result := v1.ProbeResult{Target: target, CheckedAt: checkedAt}
	if c == nil {
		result.Error = "error"
		return result
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u, err := url.Parse(target)
	if err != nil {
		result.Error = "error"
		return result
	}

	var latency time.Duration
	switch strings.ToLower(u.Scheme) {
	case "dns":
		started := time.Now()
		var addresses []net.IPAddr
		addresses, err = c.lookupIPAddr(probeCtx, u.Hostname())
		latency = time.Since(started)
		if err != nil || len(addresses) == 0 {
			err = dnsProbeError{err}
		}
	case "tcp":
		latency, err = c.checkTCP(probeCtx, u.Host)
	case "http", "https":
		var status int
		latency, status, result.TLSExpiresAt, err = c.checkHTTP(probeCtx, target, expected)
		if status != 0 {
			result.StatusCode = &status
		}
	case "icmp":
		latency, err = checkICMP(probeCtx, u.Hostname())
	default:
		result.Error = "unsupported"
		return result
	}

	if err != nil {
		result.Error = classifyError(err)
		return result
	}
	ms := float64(latency) / float64(time.Millisecond)
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 {
		result.Error = "error"
		result.StatusCode = nil
		return result
	}
	if ms > 60000 {
		ms = 60000
	}
	result.OK = true
	result.LatencyMS = &ms
	return result
}

func (c *Checker) checkTCP(ctx context.Context, address string) (time.Duration, error) {
	started := time.Now()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	latency := time.Since(started)
	if err != nil {
		return 0, err
	}
	_ = connection.Close()
	return latency, nil
}

type httpStatusError struct{}
type tlsProbeError struct{ error }
type dnsProbeError struct{ error }

func (err dnsProbeError) Error() string { return "DNS lookup failure" }
func (err dnsProbeError) Unwrap() error { return err.error }

type errorHolder struct{ error }

func (err tlsProbeError) Unwrap() error { return err.error }

func (c *Checker) checkHTTP(ctx context.Context, target string, expected int) (time.Duration, int, *time.Time, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	request.Header.Set("User-Agent", "OrbitBeacon-Probe/"+buildinfo.Version)
	var handshakeError atomic.Pointer[errorHolder]
	trace := &httptrace.ClientTrace{TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
		if err != nil {
			handshakeError.Store(&errorHolder{err})
		}
	}}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))

	started := time.Now()
	response, err := c.client.Do(request)
	latency := time.Since(started)
	if err != nil {
		if failure := handshakeError.Load(); failure != nil {
			return 0, 0, nil, tlsProbeError{failure.error}
		}
		return 0, 0, nil, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxHTTPBody))
	var expiry *time.Time
	if request.URL.Scheme == "https" && response.TLS != nil && len(response.TLS.PeerCertificates) > 0 {
		value := response.TLS.PeerCertificates[0].NotAfter
		expiry = &value
	}
	if response.StatusCode < 100 || response.StatusCode > 599 {
		return 0, 0, expiry, errors.New("invalid HTTP status code")
	}
	if expected != 0 && response.StatusCode != expected || expected == 0 && response.StatusCode >= http.StatusBadRequest {
		return 0, response.StatusCode, expiry, httpStatusError{}
	}
	return latency, response.StatusCode, expiry, nil
}

func (httpStatusError) Error() string { return "HTTP status failure" }

func classifyError(err error) string {
	if err == nil {
		return ""
	}
	var statusError httpStatusError
	if errors.As(err, &statusError) {
		return "http_status"
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return "timeout"
	}
	var lookupError dnsProbeError
	if errors.As(err, &lookupError) {
		return "dns"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "refused"
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "dns"
	}
	if isTLSError(err) {
		return "tls"
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return "unreachable"
	}
	if errors.Is(err, errICMPUnsupported) {
		return "unsupported"
	}
	return "error"
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func isTLSError(err error) bool {
	var probeError tlsProbeError
	var recordHeader tls.RecordHeaderError
	var verification *tls.CertificateVerificationError
	var alert tls.AlertError
	var unknownAuthority x509.UnknownAuthorityError
	var certificateInvalid x509.CertificateInvalidError
	var hostnameError x509.HostnameError
	var systemRoots x509.SystemRootsError
	var constraintViolation x509.ConstraintViolationError
	var insecureAlgorithm x509.InsecureAlgorithmError
	var criticalExtension x509.UnhandledCriticalExtension
	return errors.As(err, &probeError) ||
		errors.As(err, &recordHeader) ||
		errors.As(err, &verification) ||
		errors.As(err, &alert) ||
		errors.As(err, &unknownAuthority) ||
		errors.As(err, &certificateInvalid) ||
		errors.As(err, &hostnameError) ||
		errors.As(err, &systemRoots) ||
		errors.As(err, &constraintViolation) ||
		errors.As(err, &insecureAlgorithm) ||
		errors.As(err, &criticalExtension)
}
