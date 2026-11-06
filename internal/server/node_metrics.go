package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

var nodeMetricFamilies = [...]struct{ name, help string }{
	{"info", "Node metadata."},
	{"up", "Whether node presence is online."},
	{"last_seen_timestamp_seconds", "Last report receipt time as Unix seconds."},
	{"cpu_percent", "CPU usage percent."},
	{"load1", "One minute load average."},
	{"load5", "Five minute load average."},
	{"load15", "Fifteen minute load average."},
	{"memory_total_bytes", "Total memory bytes."},
	{"memory_available_bytes", "Available memory bytes."},
	{"swap_total_bytes", "Total swap bytes."},
	{"swap_used_bytes", "Used swap bytes."},
	{"uptime_seconds", "Reported uptime seconds."},
	{"disk_total_bytes", "Total disk bytes."},
	{"disk_used_bytes", "Used disk bytes."},
	{"network_receive_bytes_per_second", "Network receive bytes per second."},
	{"network_transmit_bytes_per_second", "Network transmit bytes per second."},
}

func (h *handler) serveNodeMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.ingest == nil || h.nodes == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// 两次批量读取及编码全部成功后才写响应；身份变更期间重试完整投影。
	body, _, err := consoleSnapshot(h, r.Context(), h.nodeMetrics)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

type nodeMetricSnapshot struct {
	node     nodes.LiveNode
	metadata nodes.MetricsMetadata
	sample   *ingest.LatestReport
}

func (h *handler) nodeMetrics(ctx context.Context) (string, error) {
	records, err := h.nodes.LiveRecords(ctx)
	if err != nil {
		return "", err
	}
	metadata, err := h.nodes.MetricsMetadataRecords(ctx)
	if err != nil {
		return "", err
	}
	byID := make(map[string]nodes.MetricsMetadata, len(metadata))
	for _, record := range metadata {
		byID[record.ID] = record
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	now := h.now()
	snapshots := make([]nodeMetricSnapshot, 0, len(records))
	for i := range records {
		record := &records[i]
		meta, ok := byID[record.ID]
		if !ok {
			continue // 节点在两次读取之间被删除。
		}
		snapshot := nodeMetricSnapshot{node: h.ingest.ProjectLiveRecord(record, now), metadata: meta}
		if snapshot.node.Presence == "online" {
			snapshot.sample, err = h.ingest.ProjectLiveSample(record)
			if err != nil {
				return "", err
			}
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return nodeMetricsExposition(snapshots), nil
}

func nodeMetricLabel(value string) string {
	return prometheusLabel(strings.ToValidUTF8(value, "\uFFFD"))
}

func nodeMetricsExposition(snapshots []nodeMetricSnapshot) string {
	// 各族独立缓冲，确保 HELP/TYPE 唯一且同族样本连续。
	families := make(map[string]*strings.Builder, len(nodeMetricFamilies))
	for _, family := range nodeMetricFamilies {
		families[family.name] = new(strings.Builder)
	}
	add := func(name, labels, value string) {
		fmt.Fprintf(families[name], "orbitbeacon_node_%s{%s} %s\n", name, labels, value)
	}
	for _, snapshot := range snapshots {
		node, meta := snapshot.node, snapshot.metadata
		labels := "node_id=" + nodeMetricLabel(node.ID)
		add("info", labels+",name="+nodeMetricLabel(meta.Name)+",region="+nodeMetricLabel(meta.Region)+",provider="+nodeMetricLabel(meta.Provider)+",group="+nodeMetricLabel(meta.Group)+",agent_version="+nodeMetricLabel(node.AgentVersion), "1")
		up := "0"
		if node.Presence == "online" {
			up = "1"
		}
		add("up", labels, up)
		if node.LastSeen != nil {
			seconds := float64(node.LastSeen.Unix()) + float64(node.LastSeen.Nanosecond())/1e9
			add("last_seen_timestamp_seconds", labels, strconv.FormatFloat(seconds, 'g', -1, 64))
		}
		if snapshot.sample == nil {
			continue
		}
		floatMetric := func(name, labels string, value *float64) {
			if value != nil {
				add(name, labels, strconv.FormatFloat(*value, 'g', -1, 64))
			}
		}
		uintMetric := func(name, labels string, value *uint64) {
			if value != nil {
				add(name, labels, strconv.FormatUint(*value, 10))
			}
		}
		m := snapshot.sample.Metrics
		floatMetric("cpu_percent", labels, m.CPUPercent)
		floatMetric("load1", labels, m.Load1)
		floatMetric("load5", labels, m.Load5)
		floatMetric("load15", labels, m.Load15)
		uintMetric("memory_total_bytes", labels, m.MemoryTotalBytes)
		uintMetric("memory_available_bytes", labels, m.MemoryAvailableBytes)
		uintMetric("swap_total_bytes", labels, m.SwapTotalBytes)
		uintMetric("swap_used_bytes", labels, m.SwapUsedBytes)
		floatMetric("uptime_seconds", labels, m.UptimeSeconds)
		sort.Slice(m.Disks, func(i, j int) bool { return m.Disks[i].Path < m.Disks[j].Path })
		for _, disk := range m.Disks {
			diskLabels := labels + ",path=" + nodeMetricLabel(disk.Path)
			uintMetric("disk_total_bytes", diskLabels, disk.TotalBytes)
			uintMetric("disk_used_bytes", diskLabels, disk.UsedBytes)
		}
		sort.Slice(m.Networks, func(i, j int) bool { return m.Networks[i].Name < m.Networks[j].Name })
		for _, network := range m.Networks {
			networkLabels := labels + ",interface=" + nodeMetricLabel(network.Name)
			floatMetric("network_receive_bytes_per_second", networkLabels, network.RxBytesPerSec)
			floatMetric("network_transmit_bytes_per_second", networkLabels, network.TxBytesPerSec)
		}
	}
	var body strings.Builder
	for _, family := range nodeMetricFamilies {
		fmt.Fprintf(&body, "# HELP orbitbeacon_node_%s %s\n# TYPE orbitbeacon_node_%s gauge\n", family.name, family.help, family.name)
		body.WriteString(families[family.name].String())
	}
	return body.String()
}
