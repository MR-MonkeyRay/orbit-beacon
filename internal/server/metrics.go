package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var durationBuckets = [...]float64{0.005, 0.025, 0.1, 0.5, 1, 5, 15, 30}
var flushEntryBuckets = [...]uint64{1, 10, 100, 1000, 10000, 100000}

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

// MetricsHandler 有意与 ServeHTTP 分离：公开监听器绝不暴露 /metrics。
// 调用方决定是否存在 metrics 监听器。
func (h *handler) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			h.serveMetrics(w, r)
		case "/metrics/nodes":
			h.serveNodeMetrics(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (h *handler) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	body := h.metrics.exposition()
	if h.saturation != nil {
		body += h.saturation.exposition(h)
	}
	if h.ingest != nil {
		body += fmt.Sprintf("# HELP orbitbeacon_history_samples_dropped_total Accepted reports omitted because the history buffer was full.\n# TYPE orbitbeacon_history_samples_dropped_total counter\norbitbeacon_history_samples_dropped_total %d\n", h.ingest.SamplesDropped())
	}
	if h.history != nil {
		body += fmt.Sprintf("# HELP orbitbeacon_history_series_rejected_total Partitioned samples rejected by the per-node path limit or unavailable path inventory.\n# TYPE orbitbeacon_history_series_rejected_total counter\norbitbeacon_history_series_rejected_total %d\n", h.history.SeriesRejected())
		body += fmt.Sprintf("# HELP orbitbeacon_history_buckets_dropped_total Samples omitted because pending history buckets reached capacity.\n# TYPE orbitbeacon_history_buckets_dropped_total counter\norbitbeacon_history_buckets_dropped_total %d\n", h.history.DroppedBuckets())
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
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
	// 对完整的样本行排序，使输出对运维人员和测试保持稳定。
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

// SaturationMetrics 只在启用独立指标监听时创建；写路径不获取采集锁。
// history 的单个写入任务发布不可变快照，抓取不会与刷写争用锁。
type SaturationMetrics struct {
	writer, reader *sql.DB
	dataDir        string
	reportBusy     atomic.Uint64
	flush          atomic.Pointer[historyFlushMetric]
}

type historyFlushMetric struct {
	duration     durationMetric
	entries      uint64
	entryBuckets [len(flushEntryBuckets)]uint64
}

func NewSaturationMetrics(writer, reader *sql.DB, dataDir string) *SaturationMetrics {
	return &SaturationMetrics{writer: writer, reader: reader, dataDir: dataDir}
}

// ObserveHistoryFlush 由单个 history 写入任务调用，记录非空刷写尝试（含失败）。
// entries 是三个聚合层级的计划行数。
func (m *SaturationMetrics) ObserveHistoryFlush(elapsed time.Duration, entries int) {
	next := &historyFlushMetric{}
	if previous := m.flush.Load(); previous != nil {
		*next = *previous
	}
	next.entries += uint64(entries)
	for i, bound := range flushEntryBuckets {
		if uint64(entries) <= bound {
			next.entryBuckets[i]++
		}
	}
	next.duration.count++
	next.duration.sum += elapsed.Seconds()
	for i, bound := range durationBuckets {
		if elapsed.Seconds() <= bound {
			next.duration.buckets[i]++
		}
	}
	m.flush.Store(next)
}

func (m *SaturationMetrics) exposition(h *handler) string {
	var b strings.Builder
	for _, metric := range []struct{ name, help, kind string }{
		{"orbitbeacon_db_connections_in_use", "Connections currently in use.", "gauge"},
		{"orbitbeacon_db_wait_count_total", "Connection pool waits.", "counter"},
		{"orbitbeacon_db_wait_duration_seconds_total", "Time spent waiting for a connection.", "counter"},
	} {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", metric.name, metric.help, metric.name, metric.kind)
	}
	writeStats := func(database, role string, stats sql.DBStats) {
		labels := fmt.Sprintf("database=%q,role=%q", database, role)
		fmt.Fprintf(&b, "orbitbeacon_db_connections_in_use{%s} %d\n", labels, stats.InUse)
		fmt.Fprintf(&b, "orbitbeacon_db_wait_count_total{%s} %d\n", labels, stats.WaitCount)
		fmt.Fprintf(&b, "orbitbeacon_db_wait_duration_seconds_total{%s} %g\n", labels, stats.WaitDuration.Seconds())
	}
	if m.writer != nil {
		writeStats("main", "writer", m.writer.Stats())
	}
	if m.reader != nil {
		writeStats("main", "reader", m.reader.Stats())
	}
	if h.history != nil {
		writer, reader := h.history.DatabaseStats()
		writeStats("metrics", "writer", writer)
		writeStats("metrics", "reader", reader)
	}
	if h.ingest != nil {
		fmt.Fprintf(&b, "# HELP orbitbeacon_ingest_write_queue_depth Occupied write slots, including the active writer.\n# TYPE orbitbeacon_ingest_write_queue_depth gauge\norbitbeacon_ingest_write_queue_depth %d\n", h.ingest.WriteQueueDepth())
	}
	fmt.Fprintf(&b, "# HELP orbitbeacon_agent_report_busy_total Reports rejected with report_busy; also counted as unavailable.\n# TYPE orbitbeacon_agent_report_busy_total counter\norbitbeacon_agent_report_busy_total %d\n", m.reportBusy.Load())
	flush := m.flush.Load()
	if flush == nil {
		flush = &historyFlushMetric{}
	}
	fmt.Fprintln(&b, "# HELP orbitbeacon_history_flush_duration_seconds Nonempty history flush attempt duration, including failures.\n# TYPE orbitbeacon_history_flush_duration_seconds histogram")
	for i, bound := range durationBuckets {
		fmt.Fprintf(&b, "orbitbeacon_history_flush_duration_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(bound, 'g', -1, 64), flush.duration.buckets[i])
	}
	fmt.Fprintf(&b, "orbitbeacon_history_flush_duration_seconds_bucket{le=\"+Inf\"} %d\norbitbeacon_history_flush_duration_seconds_sum %g\norbitbeacon_history_flush_duration_seconds_count %d\n", flush.duration.count, flush.duration.sum, flush.duration.count)
	fmt.Fprintln(&b, "# HELP orbitbeacon_history_flush_entries Planned aggregate rows across all tiers per nonempty flush attempt, including failures.\n# TYPE orbitbeacon_history_flush_entries histogram")
	for i, bound := range flushEntryBuckets {
		fmt.Fprintf(&b, "orbitbeacon_history_flush_entries_bucket{le=%q} %d\n", strconv.FormatUint(bound, 10), flush.entryBuckets[i])
	}
	fmt.Fprintf(&b, "orbitbeacon_history_flush_entries_bucket{le=\"+Inf\"} %d\norbitbeacon_history_flush_entries_sum %d\norbitbeacon_history_flush_entries_count %d\n", flush.duration.count, flush.entries, flush.duration.count)
	fmt.Fprintln(&b, "# HELP orbitbeacon_db_wal_size_bytes WAL file size at scrape time; absent files are zero, other stat errors omit the sample.\n# TYPE orbitbeacon_db_wal_size_bytes gauge")
	for _, db := range []struct{ name, file string }{{"main", "orbitbeacon.db-wal"}, {"metrics", "metrics.db-wal"}} {
		info, err := os.Stat(filepath.Join(m.dataDir, db.file))
		if err == nil {
			fmt.Fprintf(&b, "orbitbeacon_db_wal_size_bytes{database=%q} %d\n", db.name, info.Size())
		} else if os.IsNotExist(err) {
			fmt.Fprintf(&b, "orbitbeacon_db_wal_size_bytes{database=%q} 0\n", db.name)
		}
	}
	return b.String()
}
