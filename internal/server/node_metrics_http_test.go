package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func scrapeNodeMetrics(t *testing.T, f *agentFixture, method string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	f.handler.(interface{ MetricsHandler() http.Handler }).MetricsHandler().ServeHTTP(response, httptest.NewRequest(method, "/metrics/nodes", nil))
	return response
}

func nodeMetricsReport(t *testing.T) v1.Report {
	t.Helper()
	report := mustReport(t, reportBody(1, 1, "metrics-session", 25))
	large := uint64(1<<53 + 1)
	used := uint64(123)
	rx, tx := 1.2345678901234567, 42.0
	report.System = &v1.System{Hostname: "secret-hostname-value", OS: "linux", Arch: "amd64", AgentVersion: "v-test"}
	report.Runtime.ProbeTargets = []string{"tcp://secret-probe.example:443"}
	report.Runtime.DiskPaths = []string{"/secret-runtime-path"}
	report.Metrics.MemoryTotalBytes = &large
	report.Metrics.Disks = []v1.DiskMetrics{{Path: "/z", TotalBytes: &large, UsedBytes: &used, SampledAt: report.SampledAt}, {Path: "/a", TotalBytes: &large, UsedBytes: &used, SampledAt: report.SampledAt}}
	report.Metrics.Networks = []v1.NetworkMetrics{{Name: "zeth", RxBytesPerSec: &rx, TxBytesPerSec: &tx}, {Name: "aeth", RxBytesPerSec: &rx, TxBytesPerSec: &tx}}
	window := int64(10000)
	for _, key := range []string{"disk:/z", "disk:/a", "network:zeth", "network:aeth"} {
		report.Metrics.MetricStatus[key] = "ok"
		report.Metrics.Sources[key] = v1.SourceMetadata{SampleID: key, SourceIntervalMS: 10000, MeasurementWindowMS: &window}
	}
	return report
}

func TestNodeMetricsCompleteOutputAndHTTP(t *testing.T) {
	f := newAgentHTTP(t, 5)
	id, token, _ := enrollHTTP(t, f, "metrics-node", 1)
	group := "00000000-0000-4000-8000-000000000099"
	if _, err := f.db.Exec(`INSERT INTO groups(id,name) VALUES(?, 'fleet')`, group); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE nodes SET region='earth',provider='cloud',group_id=?,notes='secret-notes-value' WHERE id=?`, group, id); err != nil {
		t.Fatal(err)
	}
	if _, err := acceptReport(context.Background(), f.ingest, token, nodeMetricsReport(t)); err != nil {
		t.Fatal(err)
	}
	response := scrapeNodeMetrics(t, f, http.MethodGet)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	const expected = `# HELP orbitbeacon_node_info Node metadata.
# TYPE orbitbeacon_node_info gauge
orbitbeacon_node_info{node_id="ID",name="metrics-node",region="earth",provider="cloud",group="fleet",agent_version="v-test"} 1
# HELP orbitbeacon_node_up Whether node presence is online.
# TYPE orbitbeacon_node_up gauge
orbitbeacon_node_up{node_id="ID"} 1
# HELP orbitbeacon_node_last_seen_timestamp_seconds Last report receipt time as Unix seconds.
# TYPE orbitbeacon_node_last_seen_timestamp_seconds gauge
orbitbeacon_node_last_seen_timestamp_seconds{node_id="ID"} 1.793016e+09
# HELP orbitbeacon_node_cpu_percent CPU usage percent.
# TYPE orbitbeacon_node_cpu_percent gauge
orbitbeacon_node_cpu_percent{node_id="ID"} 25
# HELP orbitbeacon_node_load1 One minute load average.
# TYPE orbitbeacon_node_load1 gauge
orbitbeacon_node_load1{node_id="ID"} 0.5
# HELP orbitbeacon_node_load5 Five minute load average.
# TYPE orbitbeacon_node_load5 gauge
orbitbeacon_node_load5{node_id="ID"} 0.5
# HELP orbitbeacon_node_load15 Fifteen minute load average.
# TYPE orbitbeacon_node_load15 gauge
orbitbeacon_node_load15{node_id="ID"} 0.5
# HELP orbitbeacon_node_memory_total_bytes Total memory bytes.
# TYPE orbitbeacon_node_memory_total_bytes gauge
orbitbeacon_node_memory_total_bytes{node_id="ID"} 9007199254740993
# HELP orbitbeacon_node_memory_available_bytes Available memory bytes.
# TYPE orbitbeacon_node_memory_available_bytes gauge
orbitbeacon_node_memory_available_bytes{node_id="ID"} 4096
# HELP orbitbeacon_node_swap_total_bytes Total swap bytes.
# TYPE orbitbeacon_node_swap_total_bytes gauge
orbitbeacon_node_swap_total_bytes{node_id="ID"} 0
# HELP orbitbeacon_node_swap_used_bytes Used swap bytes.
# TYPE orbitbeacon_node_swap_used_bytes gauge
orbitbeacon_node_swap_used_bytes{node_id="ID"} 0
# HELP orbitbeacon_node_uptime_seconds Reported uptime seconds.
# TYPE orbitbeacon_node_uptime_seconds gauge
orbitbeacon_node_uptime_seconds{node_id="ID"} 100
# HELP orbitbeacon_node_disk_total_bytes Total disk bytes.
# TYPE orbitbeacon_node_disk_total_bytes gauge
orbitbeacon_node_disk_total_bytes{node_id="ID",path="/a"} 9007199254740993
orbitbeacon_node_disk_total_bytes{node_id="ID",path="/z"} 9007199254740993
# HELP orbitbeacon_node_disk_used_bytes Used disk bytes.
# TYPE orbitbeacon_node_disk_used_bytes gauge
orbitbeacon_node_disk_used_bytes{node_id="ID",path="/a"} 123
orbitbeacon_node_disk_used_bytes{node_id="ID",path="/z"} 123
# HELP orbitbeacon_node_disk_inodes_total Total disk inodes.
# TYPE orbitbeacon_node_disk_inodes_total gauge
# HELP orbitbeacon_node_disk_inodes_used Used disk inodes.
# TYPE orbitbeacon_node_disk_inodes_used gauge
# HELP orbitbeacon_node_network_receive_bytes_per_second Network receive bytes per second.
# TYPE orbitbeacon_node_network_receive_bytes_per_second gauge
orbitbeacon_node_network_receive_bytes_per_second{node_id="ID",interface="aeth"} 1.2345678901234567
orbitbeacon_node_network_receive_bytes_per_second{node_id="ID",interface="zeth"} 1.2345678901234567
# HELP orbitbeacon_node_network_transmit_bytes_per_second Network transmit bytes per second.
# TYPE orbitbeacon_node_network_transmit_bytes_per_second gauge
orbitbeacon_node_network_transmit_bytes_per_second{node_id="ID",interface="aeth"} 42
orbitbeacon_node_network_transmit_bytes_per_second{node_id="ID",interface="zeth"} 42
`
	if want := strings.ReplaceAll(expected, `"ID"`, strconv.Quote(id)); response.Body.String() != want {
		t.Fatalf("unexpected exposition:\n%s\nwant:\n%s", response.Body.String(), want)
	}
	for _, secret := range []string{"secret-notes-value", "secret-hostname-value", "secret-probe.example", "secret-runtime-path", token} {
		if strings.Contains(response.Body.String(), secret) {
			t.Errorf("exposed %s", secret)
		}
	}
	if response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatal(response.Header())
	}
	head := scrapeNodeMetrics(t, f, http.MethodHead)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != response.Header().Get("Content-Length") {
		t.Fatalf("HEAD %d %v %s", head.Code, head.Header(), head.Body.String())
	}
	post := scrapeNodeMetrics(t, f, http.MethodPost)
	if post.Code != 405 || post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST %d %v", post.Code, post.Header())
	}
	main := httptest.NewRecorder()
	f.handler.ServeHTTP(main, httptest.NewRequest(http.MethodGet, "/metrics/nodes", nil))
	if main.Code != 404 {
		t.Fatalf("main listener status %d", main.Code)
	}
	own := httptest.NewRecorder()
	f.handler.(interface{ MetricsHandler() http.Handler }).MetricsHandler().ServeHTTP(own, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if own.Code != 200 || strings.Contains(own.Body.String(), "orbitbeacon_node_") {
		t.Fatal("node metrics polluted service metrics")
	}
}

func assertNodeMetricsNoValues(t *testing.T, body string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "orbitbeacon_node_info{") && !strings.HasPrefix(line, "orbitbeacon_node_up{") && !strings.HasPrefix(line, "orbitbeacon_node_last_seen_timestamp_seconds{") {
			t.Fatalf("unexpected value: %s", line)
		}
	}
}

func TestNodeMetricsInactiveAndReset(t *testing.T) {
	for _, mutation := range []string{"offline", "pending", "disabled", "revoke", "reregister"} {
		t.Run(mutation, func(t *testing.T) {
			f := newAgentHTTP(t, 2)
			id, token, _ := enrollHTTP(t, f, "metrics-node", 1)
			if _, err := acceptReport(context.Background(), f.ingest, token, nodeMetricsReport(t)); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "offline":
				f.now = f.now.Add(nodes.OnlineWindow + time.Second)
			case "disabled":
				if err := f.nodes.SetDisabled(context.Background(), id, true); err != nil {
					t.Fatal(err)
				}
			case "pending":
				if _, err := f.db.Exec("UPDATE nodes SET status='pending' WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
			case "revoke", "reregister":
				if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				if mutation == "reregister" {
					ticket, err := f.enrollments.CreateSingle(context.Background(), id)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.enrollments.Enroll(context.Background(), ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "replacement", NodeToken: agentSecret(2)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			response := scrapeNodeMetrics(t, f, http.MethodGet)
			if response.Code != 200 || !strings.Contains(response.Body.String(), fmt.Sprintf("orbitbeacon_node_up{node_id=%q} 0\n", id)) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			assertNodeMetricsNoValues(t, response.Body.String())
			hasSeen := strings.Contains(response.Body.String(), "\norbitbeacon_node_last_seen_timestamp_seconds{")
			if hasSeen != (mutation == "offline" || mutation == "pending" || mutation == "disabled") {
				t.Fatalf("unexpected last seen: %s", response.Body.String())
			}
		})
	}
}

func TestNodeMetricsReadFailureIsAtomic(t *testing.T) {
	for _, failure := range []string{"inventory", "metadata", "context"} {
		t.Run(failure, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			enrollHTTP(t, f, "metrics-node", 1)
			switch failure {
			case "inventory":
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				if _, err := f.db.Exec("DROP TABLE groups"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "context" {
				cancel()
			}
			response := httptest.NewRecorder()
			f.handler.(interface{ MetricsHandler() http.Handler }).MetricsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics/nodes", nil).WithContext(ctx))
			if response.Code != 500 || strings.Contains(response.Body.String(), "orbitbeacon_node_") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestNodeMetricsHostileLabelsThroughIngest(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "metrics-node", 1)
	name := "name\"\\\n\xff"
	if _, err := f.db.Exec("UPDATE nodes SET name=? WHERE id=?", name, id); err != nil {
		t.Fatal(err)
	}
	report := nodeMetricsReport(t)
	path := "/disk\"\\\n\xff"
	network := "eth\"\\\xff" // 协议拒绝换行网卡名；输出层的换行防护另有单元测试。
	disk := report.Metrics.Disks[0]
	disk.Path = path
	report.Metrics.Disks = []v1.DiskMetrics{disk}
	nic := report.Metrics.Networks[0]
	nic.Name = network
	report.Metrics.Networks = []v1.NetworkMetrics{nic}
	for _, key := range []string{"disk:/z", "disk:/a", "network:zeth", "network:aeth"} {
		delete(report.Metrics.MetricStatus, key)
		delete(report.Metrics.Sources, key)
	}
	window := int64(10000)
	for _, key := range []string{v1.DiskMetricKey(path), v1.NetworkMetricKey(network)} {
		report.Metrics.MetricStatus[key] = "ok"
		report.Metrics.Sources[key] = v1.SourceMetadata{SampleID: "hostile", SourceIntervalMS: 10000, MeasurementWindowMS: &window}
	}
	if _, err := acceptReport(context.Background(), f.ingest, token, report); err != nil {
		t.Fatal(err)
	}
	response := scrapeNodeMetrics(t, f, http.MethodGet)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, escaped := range []string{`name="name\"\\\n` + "\uFFFD" + `"`, `path="/disk\"\\\n` + "\uFFFD" + `"`, `interface="eth\"\\` + "\uFFFD" + `"`} {
		if !strings.Contains(response.Body.String(), escaped) {
			t.Errorf("missing escaped label %q in %s", escaped, response.Body.String())
		}
	}
}

func TestNodeMetricsPendingWithoutReport(t *testing.T) {
	f := newAgentHTTP(t, 1)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	response := scrapeNodeMetrics(t, f, http.MethodGet)
	if response.Code != 200 || !strings.Contains(response.Body.String(), fmt.Sprintf("orbitbeacon_node_up{node_id=%q} 0\n", node.ID)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	assertNodeMetricsNoValues(t, response.Body.String())
	if strings.Contains(response.Body.String(), "\norbitbeacon_node_last_seen_timestamp_seconds{") {
		t.Fatal("pending node has a last seen")
	}
}

func TestNodeMetricsReportFixtureValidation(t *testing.T) {
	if err := nodeMetricsReport(t).Validate(); err != nil {
		t.Fatal(err)
	}
}
