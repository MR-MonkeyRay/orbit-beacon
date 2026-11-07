package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestDNSLookupOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		addresses []net.IPAddr
		err       error
		want      string
	}{
		{"success", []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}, nil, ""},
		{"empty", nil, nil, "dns"}, {"failure", nil, errors.New("private resolver error"), "dns"},
		{"resolver refused", nil, syscall.ECONNREFUSED, "dns"},
		{"deadline", nil, context.DeadlineExceeded, "timeout"},
		{"resolver timeout", nil, &net.DNSError{IsTimeout: true}, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := NewChecker(time.Second)
			defer checker.Close()
			checker.lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
				if host != "example.test" {
					t.Fatalf("host=%s", host)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing deadline")
				}
				return tc.addresses, tc.err
			}
			result := checker.Check(t.Context(), "dns://example.test")
			if result.Error != tc.want || result.OK != (tc.want == "") || (result.LatencyMS != nil) != (tc.want == "") {
				t.Fatalf("result=%+v", result)
			}
			if err := result.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDNSLookupContextTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checker := NewChecker(time.Second)
		checker.lookupIPAddr = func(ctx context.Context, _ string) ([]net.IPAddr, error) { <-ctx.Done(); return nil, ctx.Err() }
		if got := checker.Check(t.Context(), "dns://example.test"); got.Error != "timeout" {
			t.Fatalf("result=%+v", got)
		}
	})
}

func TestDNSLookupLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checker := NewChecker(time.Second)
		defer checker.Close()
		checker.lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
			time.Sleep(25 * time.Millisecond)
			return []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}, nil
		}
		result := checker.Check(t.Context(), "dns://example.test")
		if !result.OK || result.LatencyMS == nil || *result.LatencyMS != 25 {
			t.Fatalf("result=%+v", result)
		}
	})
}

func TestHTTPExpectedStatus(t *testing.T) {
	for _, tc := range []struct {
		status, expected int
		want             string
	}{
		{204, 204, ""}, {302, 204, "http_status"}, {503, 204, "http_status"}, {302, 0, ""}, {503, 0, "http_status"}, {503, 503, ""},
	} {
		checker := NewChecker(time.Second)
		checker.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("private response"))}, nil
		})
		result := checker.CheckExpectedStatus(t.Context(), "http://example.test", tc.expected)
		checker.Close()
		if result.Error != tc.want || result.StatusCode == nil || *result.StatusCode != tc.status || result.TLSExpiresAt != nil {
			t.Fatalf("status=%d expected=%d result=%+v", tc.status, tc.expected, result)
		}
		if err := result.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

// Socket tests intentionally fail when this environment forbids listeners.
func TestTLSExpirySocket(t *testing.T) {
	defer func() {
		if failure := recover(); failure != nil {
			t.Fatalf("TLS expiry socket test requires a local listener: %v", failure)
		}
	}()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	for _, expected := range []int{204, 200} {
		checker := NewChecker(time.Second)
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		checker.transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		result := checker.CheckExpectedStatus(t.Context(), server.URL, expected)
		checker.Close()
		if result.TLSExpiresAt == nil || !result.TLSExpiresAt.Equal(server.Certificate().NotAfter) || result.OK != (expected == 204) {
			t.Fatalf("expected=%d result=%+v", expected, result)
		}
		if expected != 204 && (result.Error != "http_status" || result.StatusCode == nil || *result.StatusCode != 204) {
			t.Fatalf("result=%+v", result)
		}
	}
	expired := NewChecker(time.Second)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	expired.transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, Time: func() time.Time { return server.Certificate().NotAfter.Add(time.Hour) }}
	expiredResult := expired.Check(t.Context(), server.URL)
	expired.Close()
	if expiredResult.Error != "tls" || expiredResult.TLSExpiresAt != nil {
		t.Fatalf("expired certificate result=%+v", expiredResult)
	}
	checker := NewChecker(time.Second)
	defer checker.Close()
	result := checker.Check(t.Context(), server.URL)
	if result.Error != "tls" || result.TLSExpiresAt != nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestRunnerExpectedStatusReloadDiscardsOldCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := "https://example.test"
		started, releaseOld := make(chan struct{}), make(chan struct{})
		var calls atomic.Int64
		expiry := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
		runner := NewRunner(Options{Check: func(ctx context.Context, target string) v1.ProbeResult {
			code := 200
			if calls.Add(1) == 1 {
				close(started)
				<-releaseOld
				code = 204
			}
			latency := 1.0
			return v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, CheckedAt: time.Now(), StatusCode: &code, TLSExpiresAt: &expiry}
		}})
		expected := map[string]int{target: 204}
		if err := runner.SetConfig([]string{target}, expected); err != nil {
			t.Fatal(err)
		}
		expected[target] = 500
		if runner.targets[target].expected != 204 {
			t.Fatal("map aliases caller")
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		<-started
		if err := runner.SetConfig([]string{target}, map[string]int{target: 200}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		latest := runner.Results([]string{target})
		if len(latest) != 1 || *latest[0].StatusCode != 200 {
			t.Fatalf("new check not published: %+v", latest)
		}
		*latest[0].TLSExpiresAt = time.Time{}
		if !runner.Results([]string{target})[0].TLSExpiresAt.Equal(time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)) {
			t.Fatal("snapshot aliases expiry")
		}
		close(releaseOld)
		synctest.Wait()
		latest = runner.Results([]string{target})
		if runner.targets[target].expected != 200 || len(latest) != 1 || *latest[0].StatusCode != 200 {
			t.Fatal("old check replaced current result")
		}
		cancel()
		synctest.Wait()
		<-done
	})
}

func TestRunnerRejectsExpectedStatusForNonHTTPTarget(t *testing.T) {
	target := "tcp://example.test:443"
	if err := NewRunner(Options{}).SetConfig([]string{target}, map[string]int{target: 200}); err == nil {
		t.Fatal("expected status accepted for TCP target")
	}
}

func TestHTTPResponseTLSExpiry(t *testing.T) {
	at := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, scheme := range []string{"http", "https"} {
		for _, status := range []int{204, 302, 503} {
			checker := NewChecker(time.Second)
			checker.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("private response")), TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{NotAfter: at, Subject: pkix.Name{CommonName: "private certificate subject"}}}}}, nil
			})
			result := checker.CheckExpectedStatus(t.Context(), scheme+"://example.test", 204)
			checker.Close()
			if (result.TLSExpiresAt != nil) != (scheme == "https") {
				t.Fatalf("result=%+v", result)
			}
			if result.TLSExpiresAt != nil && !result.TLSExpiresAt.Equal(at) {
				t.Fatalf("expiry=%v", result.TLSExpiresAt)
			}
			body, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private response", "private certificate subject", "expected_status"} {
				if strings.Contains(string(body), private) {
					t.Fatalf("leaked %s", body)
				}
			}
		}
	}
}
