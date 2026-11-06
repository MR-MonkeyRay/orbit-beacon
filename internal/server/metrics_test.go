package server

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/ingest"
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
	mainNodesResponse := httptest.NewRecorder()
	h.ServeHTTP(mainNodesResponse, httptest.NewRequest(http.MethodGet, "/metrics/nodes", nil))
	if mainNodesResponse.Code != http.StatusNotFound {
		t.Fatalf("node metrics on main listener: %d", mainNodesResponse.Code)
	}
	response := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics status/format: %d %v", response.Code, response.Header())
	}
	body := response.Body.String()
	if strings.Contains(body, "orbitbeacon_node_") {
		t.Fatal("node metrics appeared in service metrics")
	}
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

func TestSaturationMetricsExposition(t *testing.T) {
	// 只创建连接池句柄，不建立数据库连接或触碰受 securefs 约束的文件。
	writer, err := sql.Open("sqlite", "unused-metrics-writer")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := sql.Open("sqlite", "unused-metrics-reader")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	dir := t.TempDir()
	wal := filepath.Join(dir, "orbitbeacon.db-wal")
	if err := os.WriteFile(wal, make([]byte, 123), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewSaturationMetrics(writer, reader, dir)
	service := ingest.NewService(nil, ingest.Options{})
	h := &handler{metrics: newHTTPMetrics("test"), saturation: m, ingest: service}
	for _, d := range []time.Duration{time.Millisecond, 20 * time.Millisecond, 600 * time.Millisecond, 40 * time.Second} {
		m.ObserveHistoryFlush(d, 12)
	}
	h.observeRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/agent/v1/report", nil), func(w http.ResponseWriter, _ *http.Request) {
		h.agentError(w, fmt.Errorf("wrapped: %w", ingest.ErrWriterBusy), "test")
	})
	h.observeRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/agent/v1/report", nil), func(w http.ResponseWriter, _ *http.Request) {
		h.agentError(w, ingest.ErrServiceClosed, "test")
	})
	response := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range []string{
		`orbitbeacon_db_connections_in_use{database="main",role="writer"} 0`,
		`orbitbeacon_db_connections_in_use{database="main",role="reader"} 0`,
		`orbitbeacon_db_wait_count_total{database="main",role="writer"} 0`,
		`orbitbeacon_db_wait_duration_seconds_total{database="main",role="reader"} 0`,
		`orbitbeacon_ingest_write_queue_depth 0`,
		`orbitbeacon_agent_report_busy_total 1`,
		`orbitbeacon_agent_reports_total{reason="unavailable"} 2`,
		`orbitbeacon_history_flush_entries_bucket{le="10"} 0`,
		`orbitbeacon_history_flush_entries_bucket{le="100"} 4`,
		`orbitbeacon_history_flush_entries_bucket{le="+Inf"} 4`,
		`orbitbeacon_history_flush_entries_sum 48`,
		`orbitbeacon_history_flush_entries_count 4`,
		`orbitbeacon_history_flush_duration_seconds_bucket{le="0.005"} 1`,
		`orbitbeacon_history_flush_duration_seconds_bucket{le="0.025"} 2`,
		`orbitbeacon_history_flush_duration_seconds_bucket{le="1"} 3`,
		`orbitbeacon_history_flush_duration_seconds_bucket{le="+Inf"} 4`,
		`orbitbeacon_history_flush_duration_seconds_count 4`,
		`orbitbeacon_history_flush_duration_seconds_sum 40.621`,
		`orbitbeacon_db_wal_size_bytes{database="main"} 123`,
		`orbitbeacon_db_wal_size_bytes{database="metrics"} 0`,
	} {
		if !strings.Contains(response.Body.String(), line+"\n") {
			t.Errorf("missing %s\n%s", line, response.Body.String())
		}
	}
	// 后续抓取读取新的文件大小，不依赖后台采样。
	if err := os.Truncate(wal, 456); err != nil {
		t.Fatal(err)
	}
	if body := m.exposition(h); !strings.Contains(body, `orbitbeacon_db_wal_size_bytes{database="main"} 456`) {
		t.Fatal(body)
	}
}

func TestSaturationMetricsDisabled(t *testing.T) {
	h := &handler{metrics: newHTTPMetrics("test")}
	h.agentError(httptest.NewRecorder(), ingest.ErrWriterBusy, "test")
	response := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(response.Body.String(), "orbitbeacon_history_flush_") {
		t.Fatal("disabled saturation metrics were collected")
	}
}

func TestSaturationMetricsConcurrentScrape(t *testing.T) {
	m := NewSaturationMetrics(nil, nil, t.TempDir())
	h := &handler{saturation: m}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 1000 {
			m.ObserveHistoryFlush(time.Millisecond, 3)
		}
	}()
	for range 100 {
		body := m.exposition(h)
		var count, bucket string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "orbitbeacon_history_flush_duration_seconds_count ") {
				count = strings.TrimPrefix(line, "orbitbeacon_history_flush_duration_seconds_count ")
			}
			if strings.HasPrefix(line, `orbitbeacon_history_flush_duration_seconds_bucket{le="+Inf"} `) {
				bucket = strings.TrimPrefix(line, `orbitbeacon_history_flush_duration_seconds_bucket{le="+Inf"} `)
			}
		}
		if count != bucket {
			t.Fatalf("inconsistent histogram count=%s bucket=%s", count, bucket)
		}
	}
	wg.Wait()
	if !strings.Contains(m.exposition(h), "orbitbeacon_history_flush_duration_seconds_count 1000\n") {
		t.Fatal("lost flush observations")
	}
}

func TestHistorySeriesRejectedCounterExposition(t *testing.T) {
	h := &handler{metrics: newHTTPMetrics("test"), history: new(history.Service)}
	response := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range []string{
		"# TYPE orbitbeacon_history_series_rejected_total counter\n",
		"orbitbeacon_history_series_rejected_total 0\n",
	} {
		if !strings.Contains(response.Body.String(), line) {
			t.Fatalf("missing %q: %s", line, response.Body)
		}
	}
}
