package ingest

import (
	"context"
	"math"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

// PublicNode is a strict anonymous projection, independent of internal inventory.
type PublicNode struct {
	Name          string   `json:"name"`
	Region        string   `json:"region"`
	Provider      string   `json:"provider"`
	Status        string   `json:"status"`
	CPUPercent    *float64 `json:"cpu_percent"`
	MemoryPercent *float64 `json:"memory_percent"`
	Load1         *float64 `json:"load1"`
	UptimeSeconds *float64 `json:"uptime_seconds"`
}

type publicMetric struct {
	value      float64
	valid      bool
	ageMS      int64
	intervalMS int64
}

type publicMetrics struct {
	cpu, memory, load, uptime publicMetric
	receivedAt                time.Time
}

func metric(value *float64, status string, source v1.SourceMetadata) publicMetric {
	m := publicMetric{ageMS: source.AgeMS, intervalMS: source.SourceIntervalMS}
	if value != nil && status == "ok" {
		m.value = *value
		m.valid = true
	}
	return m
}

func snapshotPublic(m v1.Metrics, now time.Time) publicMetrics {
	p := publicMetrics{receivedAt: now,
		cpu:    metric(m.CPUPercent, m.MetricStatus[v1.MetricCPUPercent], m.Sources["cpu"]),
		load:   metric(m.Load1, m.MetricStatus[v1.MetricLoad1], m.Sources["load"]),
		uptime: metric(m.UptimeSeconds, m.MetricStatus[v1.MetricUptimeSeconds], m.Sources["uptime"])}
	if m.MemoryTotalBytes != nil && m.MemoryAvailableBytes != nil && *m.MemoryTotalBytes > 0 && m.MetricStatus[v1.MetricMemoryTotalBytes] == "ok" && m.MetricStatus[v1.MetricMemoryAvailableBytes] == "ok" {
		percent := 100 * (float64(*m.MemoryTotalBytes-*m.MemoryAvailableBytes) / float64(*m.MemoryTotalBytes))
		p.memory = metric(&percent, "ok", m.Sources["memory"])
	}
	return p
}

func (m publicMetric) fresh(elapsedMS int64) bool {
	if !m.valid || m.ageMS < 0 || m.intervalMS <= 0 {
		return false
	}
	if m.intervalMS > math.MaxInt64/3 {
		return true
	}
	limit := max(3*m.intervalMS, int64(45000))
	return m.ageMS <= limit && elapsedMS <= limit-m.ageMS
}

func (p publicMetrics) project(out *PublicNode, now time.Time) {
	if p.receivedAt.IsZero() || p.receivedAt.After(now) {
		return
	}
	elapsed := now.Sub(p.receivedAt).Milliseconds()
	if p.cpu.fresh(elapsed) {
		value := p.cpu.value
		out.CPUPercent = &value
	}
	if p.memory.fresh(elapsed) {
		value := p.memory.value
		out.MemoryPercent = &value
	}
	if p.load.fresh(elapsed) {
		value := p.load.value
		out.Load1 = &value
	}
	if p.uptime.fresh(elapsed) {
		value := p.uptime.value
		out.UptimeSeconds = &value
	}
}

// PublicNode revalidates publication and runtime ownership under the per-node
// lock used by report commits. It never creates state or decodes raw reports.
func (s *Service) PublicNode(ctx context.Context, nodeID string, now time.Time) (PublicNode, error) {
	state := s.acquireExistingState(nodeID)
	if state != nil {
		defer s.releaseState(state)
		state.mu.Lock()
		defer state.mu.Unlock()
	}
	record, err := nodes.ReadPublicRecord(ctx, s.db, nodeID)
	if err != nil {
		return PublicNode{}, err
	}
	out := PublicNode{Name: record.Settings.Name, Region: record.Settings.Region, Provider: record.Settings.Provider, Status: "unknown"}
	if record.Generation == 0 || record.Session == "" || record.LastSeen.IsZero() || record.LastSeen.After(now) {
		return out, nil
	}
	out.Status = "offline"
	if now.Sub(record.LastSeen) > 180*time.Second {
		return out, nil
	}
	out.Status = "online"
	if state != nil && state.credential == record.Credential && state.generation == record.Generation && state.session == record.Session && state.public.receivedAt.Equal(record.LastSeen) {
		state.public.project(&out, now)
	}
	return out, nil
}
