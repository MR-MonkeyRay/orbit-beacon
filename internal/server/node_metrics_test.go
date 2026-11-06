package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

// 只统计只读 SQL 的驱动；不模拟 ingest 内存样本。
type nodeMetricsBatchConnector struct {
	queries      *[]string
	failMetadata bool
	deleted      int
}

func (c nodeMetricsBatchConnector) Connect(context.Context) (driver.Conn, error) {
	return nodeMetricsBatchConn{c}, nil
}
func (nodeMetricsBatchConnector) Driver() driver.Driver { return publicStreamDriver{} }

type nodeMetricsBatchConn struct{ nodeMetricsBatchConnector }

func (nodeMetricsBatchConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (nodeMetricsBatchConn) Close() error                        { return nil }
func (nodeMetricsBatchConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c nodeMetricsBatchConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	*c.queries = append(*c.queries, query)
	if strings.Contains(query, "LEFT JOIN groups") {
		if c.failMetadata {
			return nil, errors.New("metadata read failed")
		}
		return &nodeMetricsMetadataRows{count: 500 - c.deleted}, nil
	}
	return &nodeMetricsInventoryRows{}, nil
}

type nodeMetricsInventoryRows struct{ index int }

func (*nodeMetricsInventoryRows) Columns() []string { return (&consoleFleetBatchRows{}).Columns() }
func (*nodeMetricsInventoryRows) Close() error      { return nil }
func (r *nodeMetricsInventoryRows) Next(dest []driver.Value) error {
	if r.index == 500 {
		return io.EOF
	}
	// 返回倒序，要求指标层自行按 id 排序。
	copy(dest, []driver.Value{fmt.Sprintf("%03d", 499-r.index), "pending", nil, "", nil, int64(0), "0", "", int64(0)})
	r.index++
	return nil
}

type nodeMetricsMetadataRows struct {
	index int
	count int
}

func (*nodeMetricsMetadataRows) Columns() []string {
	return []string{"id", "name", "region", "provider", "group"}
}
func (*nodeMetricsMetadataRows) Close() error { return nil }
func (r *nodeMetricsMetadataRows) Next(dest []driver.Value) error {
	if r.index == r.count {
		return io.EOF
	}
	copy(dest, []driver.Value{fmt.Sprintf("%03d", r.index), "node", "region", "provider", ""})
	r.index++
	return nil
}

func TestNodeMetricsSkipsNodeDeletedBetweenReads(t *testing.T) {
	var queries []string
	db := sql.OpenDB(nodeMetricsBatchConnector{queries: &queries, deleted: 1})
	defer db.Close()
	h := &handler{nodes: nodes.NewService(db, nodes.Options{}), ingest: ingest.NewService(nil, ingest.Options{}), now: time.Now}
	w := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/nodes", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if lines := parseNodeMetrics(t, w.Body.String())["orbitbeacon_node_up"]; len(lines) != 499 {
		t.Fatalf("samples=%d", len(lines))
	}
}

func TestNodeMetricsBatchReadsSortingAndAtomicFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		var queries []string
		db := sql.OpenDB(nodeMetricsBatchConnector{queries: &queries, failMetadata: failure})
		h := &handler{nodes: nodes.NewService(db, nodes.Options{}), ingest: ingest.NewService(nil, ingest.Options{}), now: time.Now}
		w := httptest.NewRecorder()
		h.MetricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/nodes", nil))
		db.Close()
		if len(queries) != 2 {
			t.Fatalf("queries=%d want 2", len(queries))
		}
		for _, query := range queries {
			if strings.Contains(query, "SELECT *") || strings.Contains(query, "notes") {
				t.Fatalf("unexpected SQL: %s", query)
			}
		}
		if failure {
			if w.Code != 500 || w.Body.Len() != 0 {
				t.Fatalf("partial result: %d %s", w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		values := parseNodeMetrics(t, w.Body.String())
		for _, name := range []string{"info", "up"} {
			lines := values["orbitbeacon_node_"+name]
			if len(lines) != 500 {
				t.Fatalf("%s samples=%d", name, len(lines))
			}
			for i, line := range lines {
				if !strings.Contains(line, fmt.Sprintf("node_id=\"%03d\"", i)) {
					t.Fatalf("unsorted sample: %s", line)
				}
			}
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			// 新句柄验证 HEAD 同样计算完整结果的长度。
			db := sql.OpenDB(nodeMetricsBatchConnector{queries: &queries})
			h.nodes = nodes.NewService(db, nodes.Options{})
			response := httptest.NewRecorder()
			h.MetricsHandler().ServeHTTP(response, httptest.NewRequest(method, "/metrics/nodes", nil))
			db.Close()
			if response.Code != 200 || response.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response=%d %v", response.Code, response.Header())
			}
			if method == http.MethodHead && response.Body.Len() != 0 {
				t.Fatal("HEAD has a body")
			}
		}
	}
}

// parseNodeMetrics 校验注释、标签引号/转义、数值及指标族连续性。
func parseNodeMetrics(t *testing.T, body string) map[string][]string {
	t.Helper()
	if !utf8.ValidString(body) || !strings.HasSuffix(body, "\n") {
		t.Fatal("invalid UTF-8 or missing final newline")
	}
	sample := regexp.MustCompile(`^(orbitbeacon_node_[a-z0-9_]+)\{([a-z_]+="(?:[^"\\\n]|\\[\\"n])*"(?:,[a-z_]+="(?:[^"\\\n]|\\[\\"n])*")*)\} ([^ ]+)$`)
	help, kind := map[string]bool{}, map[string]bool{}
	values := map[string][]string{}
	current := ""
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") {
			parts := strings.SplitN(line, " ", 4)
			if len(parts) != 4 || help[parts[2]] {
				t.Fatalf("duplicate/invalid HELP: %s", line)
			}
			current = parts[2]
			help[current] = true
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			parts := strings.Fields(line)
			if len(parts) != 4 || parts[2] != current || parts[3] != "gauge" || kind[parts[2]] {
				t.Fatalf("duplicate/invalid TYPE: %s", line)
			}
			kind[current] = true
			continue
		}
		match := sample.FindStringSubmatch(line)
		if match == nil || match[1] != current || !kind[current] {
			t.Fatalf("invalid or noncontiguous sample: %q", line)
		}
		if _, err := strconv.ParseFloat(match[3], 64); err != nil {
			t.Fatalf("invalid numeric value: %s", line)
		}
		values[current] = append(values[current], line)
	}
	if len(help) != len(nodeMetricFamilies) || len(kind) != len(help) {
		t.Fatalf("family metadata help=%d type=%d", len(help), len(kind))
	}
	return values
}

func TestNodeMetricsEscapingFamiliesSortingAndNil(t *testing.T) {
	// 包括协议不接受的换行网卡名，直接检验输出边界也安全处理它。
	hostile := "evil\"\\\n\xff"
	large := uint64(1<<53 + 1)
	value := 1.2345678901234567
	snapshots := []nodeMetricSnapshot{
		{node: nodes.LiveNode{ID: "a", Presence: "online", AgentVersion: hostile}, metadata: nodes.MetricsMetadata{Name: hostile, Region: hostile, Provider: hostile, Group: hostile}, sample: &ingest.LatestReport{Report: v1.Report{Metrics: v1.Metrics{
			MemoryTotalBytes: &large,
			Disks:            []v1.DiskMetrics{{Path: "/z", TotalBytes: &large}, {Path: hostile, TotalBytes: &large}},
			Networks:         []v1.NetworkMetrics{{Name: "z", RxBytesPerSec: &value}, {Name: hostile, RxBytesPerSec: &value}},
		}}}},
		{node: nodes.LiveNode{ID: "b", Presence: "online"}, sample: &ingest.LatestReport{Report: v1.Report{Metrics: v1.Metrics{MemoryTotalBytes: &large}}}},
	}
	body := nodeMetricsExposition(snapshots)
	values := parseNodeMetrics(t, body)
	escaped := `"evil\"\\\n` + "\uFFFD" + `"`
	for _, label := range []string{"name", "region", "provider", "group", "agent_version", "path", "interface"} {
		if !strings.Contains(body, label+"="+escaped) {
			t.Errorf("missing escaped %s: %s", label, body)
		}
	}
	for _, family := range []string{"cpu_percent", "load1", "load5", "load15", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes", "uptime_seconds", "disk_used_bytes", "disk_inodes_total", "disk_inodes_used", "network_transmit_bytes_per_second", "last_seen_timestamp_seconds"} {
		if len(values["orbitbeacon_node_"+family]) != 0 {
			t.Errorf("nil field exported: %s", family)
		}
	}
	memory := values["orbitbeacon_node_memory_total_bytes"]
	if len(memory) != 2 || memory[0] != `orbitbeacon_node_memory_total_bytes{node_id="a"} 9007199254740993` || memory[1] != `orbitbeacon_node_memory_total_bytes{node_id="b"} 9007199254740993` {
		t.Fatalf("precision/order: %v", memory)
	}
	disks := values["orbitbeacon_node_disk_total_bytes"]
	if len(disks) != 2 || !strings.Contains(disks[0], `path="/z"`) || !strings.Contains(disks[1], "path="+escaped) {
		t.Fatalf("disk order: %v", disks)
	}
	networks := values["orbitbeacon_node_network_receive_bytes_per_second"]
	if len(networks) != 2 || !strings.Contains(networks[0], "interface="+escaped) || !strings.Contains(networks[1], `interface="z"`) {
		t.Fatalf("network order: %v", networks)
	}
	if again := nodeMetricsExposition(snapshots); again != body {
		t.Fatal("exposition is unstable")
	}
}

func TestNodeMetricsUnavailableAndMethods(t *testing.T) {
	for _, h := range []*handler{{}, {nodes: new(nodes.Service)}, {ingest: new(ingest.Service)}} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			w := httptest.NewRecorder()
			h.MetricsHandler().ServeHTTP(w, httptest.NewRequest(method, "/metrics/nodes", nil))
			want := 503
			if method == http.MethodPost {
				want = 405
				if w.Header().Get("Allow") != "GET, HEAD" {
					t.Fatal(w.Header())
				}
			}
			if w.Code != want || w.Body.Len() != 0 {
				t.Fatalf("%s: status=%d body=%s", method, w.Code, w.Body.String())
			}
		}
	}
}

func TestNodeMetricsDiskInodesAndNil(t *testing.T) {
	total, used := uint64(100), uint64(25)
	body := nodeMetricsExposition([]nodeMetricSnapshot{{node: nodes.LiveNode{ID: "node", Presence: "online"}, sample: &ingest.LatestReport{Report: v1.Report{Metrics: v1.Metrics{Disks: []v1.DiskMetrics{
		{Path: "/data:one", InodesTotal: &total, InodesUsed: &used}, {Path: "/unsupported"},
	}}}}}})
	values := parseNodeMetrics(t, body)
	for family, want := range map[string]string{"disk_inodes_total": "100", "disk_inodes_used": "25"} {
		lines := values["orbitbeacon_node_"+family]
		expected := `orbitbeacon_node_` + family + `{node_id="node",path="/data:one"} ` + want
		if len(lines) != 1 || lines[0] != expected {
			t.Fatalf("%s=%v", family, lines)
		}
	}
}

func TestNodeMetricsMaximumSeriesCount(t *testing.T) {
	value, total := 100.0, uint64(100)
	seen := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	m := v1.Metrics{CPUPercent: &value, Load1: &value, Load5: &value, Load15: &value, UptimeSeconds: &value,
		MemoryTotalBytes: &total, MemoryAvailableBytes: &total, SwapTotalBytes: &total, SwapUsedBytes: &total}
	for i := range 8 {
		m.Disks = append(m.Disks, v1.DiskMetrics{Path: fmt.Sprintf("/disk%d", i), TotalBytes: &total, UsedBytes: &total, InodesTotal: &total, InodesUsed: &total})
		m.Networks = append(m.Networks, v1.NetworkMetrics{Name: fmt.Sprintf("eth%d", i), RxBytesPerSec: &value, TxBytesPerSec: &value})
	}
	values := parseNodeMetrics(t, nodeMetricsExposition([]nodeMetricSnapshot{{node: nodes.LiveNode{ID: "node", Presence: "online", LastSeen: &seen}, sample: &ingest.LatestReport{Report: v1.Report{Metrics: m}}}}))
	var count int
	for _, lines := range values {
		count += len(lines)
	}
	if count != 60 {
		t.Fatalf("series=%d want 60", count)
	}
}
