package server

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var durationBuckets = [...]float64{0.005, 0.025, 0.1, 0.5, 1, 5, 15, 30}

type requestLabels struct{ method, route, status string }
type durationLabels struct{ method, route string }
type durationMetric struct {
	count   uint64
	sum     float64
	buckets [len(durationBuckets)]uint64
}
type httpMetrics struct {
	version       string
	inflight      atomic.Int64
	publicStreams atomic.Int64
	mu            sync.Mutex
	requests      map[requestLabels]uint64
	durations     map[durationLabels]*durationMetric
	reports       map[string]uint64
	logins        map[string]uint64
}

func newHTTPMetrics(version string) *httpMetrics {
	if version == "" {
		version = "unknown"
	}
	return &httpMetrics{version: version, requests: make(map[requestLabels]uint64), durations: make(map[durationLabels]*durationMetric), reports: make(map[string]uint64), logins: make(map[string]uint64)}
}

func metricMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method
	}
	return "OTHER"
}

func statusReason(status int) string {
	switch status {
	case 400:
		return "invalid_request"
	case 401:
		return "unauthenticated"
	case 403:
		return "forbidden"
	case 409:
		return "identity_conflict"
	case 413:
		return "body_too_large"
	case 426:
		return "protocol_unsupported"
	case 429:
		return "rate_limited"
	case 503:
		return "unavailable"
	}
	if status >= 500 {
		return "internal_error"
	}
	return "other"
}

func (m *httpMetrics) observe(method, route string, status int, elapsed time.Duration, path string) {
	method = metricMethod(method)
	seconds := elapsed.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestLabels{method, route, strconv.Itoa(status/100) + "xx"}]++
	key := durationLabels{method, route}
	d := m.durations[key]
	if d == nil {
		d = &durationMetric{}
		m.durations[key] = d
	}
	d.count++
	d.sum += seconds
	for i, bound := range durationBuckets {
		if seconds <= bound {
			d.buckets[i]++
		}
	}
	if path == "/api/agent/v1/report" && method == http.MethodPost {
		reason := statusReason(status)
		if status >= 200 && status < 300 {
			reason = "accepted"
		}
		m.reports[reason]++
	}
	if path == "/api/v1/session" && method == http.MethodPost && status >= 400 {
		reason := statusReason(status)
		if status == http.StatusUnauthorized {
			reason = "invalid_credentials"
		}
		m.logins[reason]++
	}
}

// MetricsHandler is deliberately separate from ServeHTTP: the public listener
// never exposes /metrics. The caller controls whether a metrics listener exists.
func (h *handler) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		body := h.metrics.exposition()
		if h.ingest != nil {
			body += fmt.Sprintf("# HELP orbitbeacon_history_samples_dropped_total Accepted reports omitted because the history buffer was full.\n# TYPE orbitbeacon_history_samples_dropped_total counter\norbitbeacon_history_samples_dropped_total %d\n", h.ingest.SamplesDropped())
		}
		if h.history != nil {
			body += fmt.Sprintf("# HELP orbitbeacon_history_buckets_dropped_total Samples omitted because pending history buckets reached capacity.\n# TYPE orbitbeacon_history_buckets_dropped_total counter\norbitbeacon_history_buckets_dropped_total %d\n", h.history.DroppedBuckets())
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	})
}

func prometheusLabel(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(value) + `"`
}

func (m *httpMetrics) exposition() string {
	var b strings.Builder
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Fprintln(&b, "# HELP orbitbeacon_http_requests_total Completed HTTP requests.\n# TYPE orbitbeacon_http_requests_total counter")
	m.mu.Lock()
	// Sort complete sample lines to make exposition stable for operators and tests.
	var samples []string
	for key, count := range m.requests {
		samples = append(samples, fmt.Sprintf("orbitbeacon_http_requests_total{method=%q,route=%q,status_class=%q} %d\n", key.method, key.route, key.status, count))
	}
	sort.Strings(samples)
	for _, sample := range samples {
		b.WriteString(sample)
	}
	fmt.Fprintln(&b, "# HELP orbitbeacon_http_request_duration_seconds HTTP request duration.\n# TYPE orbitbeacon_http_request_duration_seconds histogram")
	samples = nil
	for key, d := range m.durations {
		labels := fmt.Sprintf("method=%q,route=%q", key.method, key.route)
		var series strings.Builder
		for i, bound := range durationBuckets {
			fmt.Fprintf(&series, "orbitbeacon_http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(bound, 'g', -1, 64), d.buckets[i])
		}
		fmt.Fprintf(&series, "orbitbeacon_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, d.count)
		fmt.Fprintf(&series, "orbitbeacon_http_request_duration_seconds_sum{%s} %g\n", labels, d.sum)
		fmt.Fprintf(&series, "orbitbeacon_http_request_duration_seconds_count{%s} %d\n", labels, d.count)
		samples = append(samples, series.String())
	}
	sort.Strings(samples)
	for _, sample := range samples {
		b.WriteString(sample)
	}
	for _, metric := range []struct {
		name, help string
		values     map[string]uint64
	}{
		{"orbitbeacon_agent_reports_total", "Report HTTP outcomes by status-derived reason (accepted means HTTP success).", m.reports},
		{"orbitbeacon_login_rejections_total", "Rejected login HTTP requests by status-derived reason.", m.logins},
	} {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", metric.name, metric.help, metric.name)
		reasons := make([]string, 0, len(metric.values))
		for reason := range metric.values {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			fmt.Fprintf(&b, "%s{reason=%q} %d\n", metric.name, reason, metric.values[reason])
		}
	}
	m.mu.Unlock()
	fmt.Fprintf(&b, "# HELP orbitbeacon_http_requests_in_flight Active HTTP requests.\n# TYPE orbitbeacon_http_requests_in_flight gauge\norbitbeacon_http_requests_in_flight %d\n", m.inflight.Load())
	fmt.Fprintf(&b, "# HELP orbitbeacon_public_streams Open public status streams.\n# TYPE orbitbeacon_public_streams gauge\norbitbeacon_public_streams %d\n", m.publicStreams.Load())
	fmt.Fprintf(&b, "# HELP go_goroutines Current goroutines.\n# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(&b, "# HELP go_memstats_heap_alloc_bytes Allocated heap bytes.\n# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", mem.HeapAlloc)
	fmt.Fprintf(&b, "# HELP orbitbeacon_build_info Build version.\n# TYPE orbitbeacon_build_info gauge\norbitbeacon_build_info{version=%s} 1\n", prometheusLabel(m.version))
	return b.String()
}
