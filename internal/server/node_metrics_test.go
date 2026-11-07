package server

import (
	"fmt"
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
	"orbitbeacon/internal/timekey"
)

func TestNodeMetricsHoldsGenerationUntilResponseWriteCompletes(t *testing.T) {
	f := newConsoleLiveFixture(t)
	unblock, release := liveTestGate(t)
	w := &blockedLiveWriter{streamRecorder: newStreamRecorder(), entered: make(chan struct{}), unblock: unblock}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.MetricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/nodes", nil))
	}()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("metrics response did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.h.nodes.Database().Close() }()
	select {
	case err := <-closed:
		t.Fatalf("generation drained before response write completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	waitStreamDone(t, done)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("generation remained pinned after response completed")
	}
}

func TestNodeMetricsSortingAndAtomicFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(strconv.FormatBool(failure), func(t *testing.T) {
			f := newConsoleLiveFixture(t)
			tx, err := f.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for i := 499; i >= 0; i-- {
				if _, err := tx.Exec("INSERT INTO nodes(id,name,region,provider,created_at) VALUES(?, 'node', 'region', 'provider', ?)", fmt.Sprintf("%03d", i), timekey.Format(f.h.now())); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if failure {
				if _, err := f.db.Exec("DROP TABLE groups"); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			f.h.MetricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/nodes", nil))
			if failure {
				if w.Code != 500 || w.Body.Len() != 0 {
					t.Fatalf("partial result: %d %s", w.Code, w.Body.String())
				}
				return
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
				response := httptest.NewRecorder()
				f.h.MetricsHandler().ServeHTTP(response, httptest.NewRequest(method, "/metrics/nodes", nil))
				if response.Code != 200 || response.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("response=%d %v", response.Code, response.Header())
				}
				if method == http.MethodHead && response.Body.Len() != 0 {
					t.Fatal("HEAD has a body")
				}
			}
		})
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
	m := v1.Metrics{DiskIO: &v1.DiskIOMetrics{ReadBytesPerSec: value, WriteBytesPerSec: value, BusyPercent: value}, Pressure: &v1.PressureMetrics{CPU: &value, Memory: &value, IO: &value}, TemperatureCelsius: &value, CPUPercent: &value, Load1: &value, Load5: &value, Load15: &value, UptimeSeconds: &value,
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
	if count != 67 {
		t.Fatalf("series=%d want 67", count)
	}
}

func TestNodeMetricsOptionalHostSeriesAndNil(t *testing.T) {
	value := 42.5
	m := v1.Metrics{DiskIO: &v1.DiskIOMetrics{ReadBytesPerSec: 1024, WriteBytesPerSec: 2048, BusyPercent: 25}, Pressure: &v1.PressureMetrics{CPU: &value, IO: &value}, TemperatureCelsius: &value}
	samples := []nodeMetricSnapshot{{node: nodes.LiveNode{ID: "node", Presence: "online"}, sample: &ingest.LatestReport{Report: v1.Report{Metrics: m}}}}
	values := parseNodeMetrics(t, nodeMetricsExposition(samples))
	for family, want := range map[string]string{"disk_read_bytes_per_second": "1024", "disk_write_bytes_per_second": "2048", "disk_busy_percent": "25", "temperature_celsius": "42.5"} {
		lines := values["orbitbeacon_node_"+family]
		if len(lines) != 1 || lines[0] != `orbitbeacon_node_`+family+`{node_id="node"} `+want {
			t.Fatalf("%s: %v", family, lines)
		}
	}
	lines := values["orbitbeacon_node_pressure_percent"]
	if len(lines) != 2 || lines[0] != `orbitbeacon_node_pressure_percent{node_id="node",resource="cpu"} 42.5` || lines[1] != `orbitbeacon_node_pressure_percent{node_id="node",resource="io"} 42.5` {
		t.Fatal(lines)
	}
	samples[0].sample.Metrics = v1.Metrics{}
	values = parseNodeMetrics(t, nodeMetricsExposition(samples))
	for _, family := range []string{"disk_read_bytes_per_second", "disk_write_bytes_per_second", "disk_busy_percent", "pressure_percent", "temperature_celsius"} {
		if len(values["orbitbeacon_node_"+family]) != 0 {
			t.Fatal(family)
		}
	}
}
