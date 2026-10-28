package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type syntheticTimeoutError struct{}

func (syntheticTimeoutError) Error() string   { return "synthetic timeout" }
func (syntheticTimeoutError) Timeout() bool   { return true }
func (syntheticTimeoutError) Temporary() bool { return true }

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", want: ""},
		{name: "deadline", err: context.DeadlineExceeded, want: "timeout"},
		{name: "timeout interface", err: syntheticTimeoutError{}, want: "timeout"},
		{name: "refused", err: fmt.Errorf("wrapped: %w", syscall.ECONNREFUSED), want: "refused"},
		{name: "DNS", err: &net.DNSError{Err: "host not found", Name: "example.invalid"}, want: "dns"},
		{name: "TLS", err: x509.UnknownAuthorityError{}, want: "tls"},
		{name: "TLS verification", err: &tls.CertificateVerificationError{Err: x509.ConstraintViolationError{}}, want: "tls"},
		{name: "TLS alert", err: tls.AlertError(42), want: "tls"},
		{name: "X509 constraint", err: x509.ConstraintViolationError{}, want: "tls"},
		{name: "X509 insecure algorithm", err: x509.InsecureAlgorithmError(0), want: "tls"},
		{name: "X509 critical extension", err: x509.UnhandledCriticalExtension{}, want: "tls"},
		{name: "network unreachable", err: &net.OpError{Err: syscall.ENETUNREACH}, want: "unreachable"},
		{name: "host unreachable", err: syscall.EHOSTUNREACH, want: "unreachable"},
		{name: "ICMP unsupported", err: errICMPUnsupported, want: "unsupported"},
		{name: "ICMP permission denied", err: icmpUnsupportedError{&os.SyscallError{Syscall: "socket", Err: syscall.EPERM}}, want: "unsupported"},
		{name: "other permission denied", err: &os.SyscallError{Syscall: "connect", Err: syscall.EPERM}, want: "error"},
		{name: "HTTP status", err: httpStatusError{}, want: "http_status"},
		{name: "generic", err: errors.New("failed"), want: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyError(test.err); got != test.want {
				t.Fatalf("classifyError(%T) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("body read failed") }

func TestHTTPHeadersDetermineSuccess(t *testing.T) {
	checker := NewChecker(time.Second)
	checker.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(failingReader{}),
		}, nil
	})
	result := checker.Check(context.Background(), "http://example.test/")
	if !result.OK || result.Error != "" || result.LatencyMS == nil || result.StatusCode == nil || *result.StatusCode != http.StatusOK {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHTTPInvalidStatusIsGenericFailure(t *testing.T) {
	checker := NewChecker(time.Second)
	checker.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 600,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	result := checker.Check(context.Background(), "http://example.test/")
	if result.OK || result.Error != "error" || result.LatencyMS != nil || result.StatusCode != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHTTPGenericTLSHandshakeFailure(t *testing.T) {
	checker := NewChecker(time.Second)
	checker.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		err := errors.New("synthetic handshake failure")
		httptrace.ContextClientTrace(request.Context()).TLSHandshakeDone(tls.ConnectionState{}, err)
		return nil, err
	})
	result := checker.Check(context.Background(), "https://example.test/")
	if result.OK || result.Error != "tls" || result.LatencyMS != nil || result.StatusCode != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHTTPHandshakeTimeoutTakesPrecedence(t *testing.T) {
	checker := NewChecker(time.Second)
	checker.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(request.Context()).TLSHandshakeDone(tls.ConnectionState{}, context.DeadlineExceeded)
		return nil, context.DeadlineExceeded
	})
	result := checker.Check(context.Background(), "https://example.test/")
	if result.OK || result.Error != "timeout" || result.LatencyMS != nil || result.StatusCode != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
}
