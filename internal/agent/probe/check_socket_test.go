package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/buildinfo"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestTCPProbeLocalListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
		close(accepted)
	}()

	checker := NewChecker(time.Second)
	defer checker.Close()
	result := checker.Check(context.Background(), "tcp://"+listener.Addr().String())
	if !result.OK || result.Error != "" || result.LatencyMS == nil || result.StatusCode != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
	<-accepted
}

func TestTCPProbeRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := "tcp://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	checker := NewChecker(time.Second)
	defer checker.Close()
	result := checker.Check(context.Background(), target)
	if result.OK || result.Error != "refused" || result.LatencyMS != nil || result.StatusCode != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHTTPProbeOK(t *testing.T) {
	userAgent := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		userAgent <- request.UserAgent()
		_, _ = response.Write([]byte(strings.Repeat("x", maxHTTPBody+1)))
	}))
	defer server.Close()

	result := checkHTTPTestTarget(t, server.URL, time.Second)
	if !result.OK || result.Error != "" || result.LatencyMS == nil || result.StatusCode == nil || *result.StatusCode != http.StatusOK {
		t.Fatalf("unexpected result: %+v", result)
	}
	if got, want := <-userAgent, "OrbitBeacon-Probe/"+buildinfo.Version; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
}

func TestHTTPProbeServiceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	result := checkHTTPTestTarget(t, server.URL, time.Second)
	if result.OK || result.Error != "http_status" || result.LatencyMS != nil || result.StatusCode == nil || *result.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHTTPProbeDoesNotFollowRedirect(t *testing.T) {
	var redirected atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "/destination", http.StatusFound)
	})
	mux.HandleFunc("/destination", func(response http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		response.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result := checkHTTPTestTarget(t, server.URL, time.Second)
	if !result.OK || result.StatusCode == nil || *result.StatusCode != http.StatusFound {
		t.Fatalf("unexpected result: %+v", result)
	}
	if redirected.Load() {
		t.Fatal("probe followed redirect")
	}
}

func TestHTTPSProbeUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	result := checkHTTPTestTarget(t, server.URL, 10*time.Second)
	if result.OK || result.Error != "tls" || result.LatencyMS != nil || result.StatusCode != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHTTPProbeTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result := checkHTTPTestTarget(t, server.URL, 10*time.Millisecond)
	if result.OK || result.Error != "timeout" || result.LatencyMS != nil || result.StatusCode != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHTTPProbeTruncatedBodyUsesHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Length", "100")
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("short"))
	}))
	defer server.Close()

	result := checkHTTPTestTarget(t, server.URL, time.Second)
	if !result.OK || result.Error != "" || result.LatencyMS == nil || result.StatusCode == nil || *result.StatusCode != http.StatusOK {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func checkHTTPTestTarget(t *testing.T, target string, timeout time.Duration) v1.ProbeResult {
	t.Helper()
	checker := NewChecker(timeout)
	t.Cleanup(checker.Close)
	return checker.Check(context.Background(), target)
}
