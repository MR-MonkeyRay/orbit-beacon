package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMetricsExpositionAndHTTPIsolation(t *testing.T) {
	h := &handler{origin: "https://beacon.test", metrics: newHTTPMetrics("test"), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, target := range []struct{ method, path string }{
		{http.MethodGet, "/healthz/live"},
		{http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001"},
		{http.MethodPost, "/api/agent/v1/report"},
		{http.MethodPost, "/api/v1/session"},
		{"PRIVATE_METHOD", "/private-token"},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(target.method, target.path, nil))
	}
	// 成功的报告和被限流的登录状态路径在同一个 HTTP 边界被观测；
	// 两个指标都不需要检查请求或响应体。
	h.metrics.observe(http.MethodPost, "/api/agent/v1/report", 200, 20*time.Millisecond, "/api/agent/v1/report")
	h.metrics.observe(http.MethodPost, "/api/v1/session", 429, 600*time.Millisecond, "/api/v1/session")
	h.metrics.observe(http.MethodPost, "/api/v1/session", 401, 600*time.Millisecond, "/api/v1/session")
	mainResponse := httptest.NewRecorder()
	h.ServeHTTP(mainResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if mainResponse.Code != http.StatusNotFound {
		t.Fatalf("metrics on main listener: %d", mainResponse.Code)
	}
	response := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics status/format: %d %v", response.Code, response.Header())
	}
	body := response.Body.String()
	for _, line := range []string{
		`orbitbeacon_http_requests_total{method="GET",route="/api/v1/nodes/{id}",status_class="4xx"} 1`,
		`orbitbeacon_http_requests_total{method="OTHER",route="unmatched",status_class="4xx"} 1`,
		`orbitbeacon_http_request_duration_seconds_bucket{method="POST",route="/api/agent/v1/report",le="+Inf"} 2`,
		`orbitbeacon_http_request_duration_seconds_count{method="POST",route="/api/agent/v1/report"} 2`,
		`orbitbeacon_http_requests_in_flight 0`,
		`orbitbeacon_agent_reports_total{reason="accepted"} 1`,
		`orbitbeacon_agent_reports_total{reason="unauthenticated"} 1`,
		`orbitbeacon_login_rejections_total{reason="forbidden"} 1`,
		`orbitbeacon_login_rejections_total{reason="invalid_credentials"} 1`,
		`orbitbeacon_login_rejections_total{reason="rate_limited"} 1`,
		`go_goroutines `, `go_memstats_heap_alloc_bytes `, `orbitbeacon_build_info{version="test"} 1`,
	} {
		if !strings.Contains(body, line) {
			t.Errorf("metrics missing %q:\n%s", line, body)
		}
	}
	for _, secret := range []string{"00000000-0000-4000-8000-000000000001", "PRIVATE_METHOD", "private-token"} {
		if strings.Contains(body, secret) {
			t.Errorf("metrics contain unbounded private input: %s", secret)
		}
	}
	// 每个样本都必须是合法的 Prometheus 名称/标签集和数值。
	sample := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{.*\})? ([^ ]+)$`)
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		match := sample.FindStringSubmatch(line)
		if match == nil {
			t.Errorf("invalid sample: %q", line)
			continue
		}
		if _, err := strconv.ParseFloat(match[2], 64); err != nil {
			t.Errorf("invalid value: %q", line)
		}
	}
	for _, target := range []struct {
		method, path string
		status       int
	}{
		{http.MethodHead, "/metrics", 200}, {http.MethodPost, "/metrics", 405}, {http.MethodGet, "/", 404},
	} {
		r := httptest.NewRecorder()
		h.MetricsHandler().ServeHTTP(r, httptest.NewRequest(target.method, target.path, nil))
		if r.Code != target.status {
			t.Errorf("%s %s status = %d", target.method, target.path, r.Code)
		}
		if target.method == http.MethodHead && r.Body.Len() != 0 {
			t.Error("metrics HEAD returned body")
		}
	}
}

func TestDurationHistogramCumulativeBuckets(t *testing.T) {
	m := newHTTPMetrics("test")
	for _, d := range []time.Duration{time.Millisecond, 20 * time.Millisecond, 600 * time.Millisecond, 40 * time.Second} {
		m.observe("GET", "unmatched", 404, d, "/missing")
	}
	body := m.exposition()
	for _, expected := range []struct {
		le    string
		count int
	}{{"0.005", 1}, {"0.025", 2}, {"0.5", 2}, {"1", 3}, {"30", 3}, {"+Inf", 4}} {
		line := fmt.Sprintf("orbitbeacon_http_request_duration_seconds_bucket{method=\"GET\",route=\"unmatched\",le=%q} %d\n", expected.le, expected.count)
		if !strings.Contains(body, line) {
			t.Errorf("missing cumulative bucket %q", line)
		}
	}
}

func TestMetricsInFlightAndConcurrentObservation(t *testing.T) {
	h := &handler{metrics: newHTTPMetrics("test")}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		h.observeRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), func(w http.ResponseWriter, _ *http.Request) { close(started); <-release; w.WriteHeader(204) })
	}()
	<-started
	if !strings.Contains(h.metrics.exposition(), "orbitbeacon_http_requests_in_flight 1\n") {
		t.Error("active request not counted")
	}
	close(release)
	<-done
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				h.metrics.observe("GET", "/", 200, time.Millisecond, "/")
				_ = h.metrics.exposition()
			}
		}()
	}
	wg.Wait()
	if !strings.Contains(h.metrics.exposition(), `orbitbeacon_http_requests_total{method="GET",route="/",status_class="2xx"} 401`) {
		t.Error("concurrent requests lost")
	}
}

func TestPrometheusBuildLabelEscaping(t *testing.T) {
	if got := prometheusLabel("v1\\test\"\n"); got != `"v1\\test\"\n"` {
		t.Fatalf("invalid label escaping: %s", got)
	}
}
