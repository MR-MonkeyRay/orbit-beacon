package ingest

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

// PublicNode 是严格的匿名投影，独立于内部清单。
type PublicNode struct {
	Name          string             `json:"name"`
	Region        string             `json:"region"`
	Provider      string             `json:"provider"`
	Status        string             `json:"status"`
	LastSeen      *time.Time         `json:"last_seen"`
	CPUPercent    *float64           `json:"cpu_percent"`
	MemoryPercent *float64           `json:"memory_percent"`
	Load1         *float64           `json:"load1"`
	UptimeSeconds *float64           `json:"uptime_seconds"`
	MetricStatus  PublicMetricStatus `json:"metric_status"`
}

type PublicMetricStatus struct {
	CPUPercent    string `json:"cpu_percent"`
	MemoryPercent string `json:"memory_percent"`
	Load1         string `json:"load1"`
	UptimeSeconds string `json:"uptime_seconds"`
}

func unavailablePublicMetrics() PublicMetricStatus {
	return PublicMetricStatus{CPUPercent: "unavailable", MemoryPercent: "unavailable", Load1: "unavailable", UptimeSeconds: "unavailable"}
}

type publicMetric struct {
	value      float64
	valid      bool
	status     string
	ageMS      int64
	intervalMS int64
}

type publicMetrics struct {
	cpu, memory, load, uptime publicMetric
	receivedAt                time.Time
}

func metric(value *float64, status string, source v1.SourceMetadata, maximum float64) publicMetric {
	m := publicMetric{status: status, ageMS: source.AgeMS, intervalMS: source.SourceIntervalMS}
	if value != nil && (status == "ok" || status == "stale") && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= maximum {
		m.value = *value
		m.valid = true
	}
	return m
}

func snapshotPublic(m v1.Metrics, now time.Time) publicMetrics {
	p := publicMetrics{receivedAt: now,
		cpu:    metric(m.CPUPercent, m.MetricStatus[v1.MetricCPUPercent], m.Sources["cpu"], 100),
		load:   metric(m.Load1, m.MetricStatus[v1.MetricLoad1], m.Sources["load"], math.MaxFloat64),
		uptime: metric(m.UptimeSeconds, m.MetricStatus[v1.MetricUptimeSeconds], m.Sources["uptime"], math.MaxFloat64)}
	totalStatus, availableStatus := m.MetricStatus[v1.MetricMemoryTotalBytes], m.MetricStatus[v1.MetricMemoryAvailableBytes]
	if m.MemoryTotalBytes != nil && m.MemoryAvailableBytes != nil && *m.MemoryTotalBytes > 0 && *m.MemoryAvailableBytes <= *m.MemoryTotalBytes && (totalStatus == "ok" || totalStatus == "stale") && (availableStatus == "ok" || availableStatus == "stale") {
		percent := 100 * (float64(*m.MemoryTotalBytes-*m.MemoryAvailableBytes) / float64(*m.MemoryTotalBytes))
		status := "ok"
		if totalStatus == "stale" || availableStatus == "stale" {
			status = "stale"
		}
		p.memory = metric(&percent, status, m.Sources["memory"], 100)
	}
	return p
}

func (m publicMetric) fresh(elapsedMS int64) bool {
	if !m.valid || m.status == "stale" {
		return false
	}
	return (v1.SourceMetadata{AgeMS: m.ageMS, SourceIntervalMS: m.intervalMS}).FreshAfter(elapsedMS)
}

func (p publicMetrics) project(out *PublicNode, now time.Time) {
	out.MetricStatus = unavailablePublicMetrics()
	if p.receivedAt.IsZero() || p.receivedAt.After(now) {
		return
	}
	elapsed := now.Sub(p.receivedAt).Milliseconds()
	out.CPUPercent, out.MetricStatus.CPUPercent = p.cpu.project(elapsed)
	out.MemoryPercent, out.MetricStatus.MemoryPercent = p.memory.project(elapsed)
	out.Load1, out.MetricStatus.Load1 = p.load.project(elapsed)
	out.UptimeSeconds, out.MetricStatus.UptimeSeconds = p.uptime.project(elapsed)
}

func (m publicMetric) project(elapsedMS int64) (*float64, string) {
	if !m.valid {
		return nil, "unavailable"
	}
	status := "stale"
	if m.fresh(elapsedMS) {
		status = "ok"
	}
	return new(m.value), status
}

// ProjectPublicRecord 仅对相同持久化身份叠加上接收时间。
// 若在页面查询之后提交了持久化报告，则省略指标。
func (s *Service) ProjectPublicRecord(record nodes.PublicRecord, now time.Time) PublicNode {
	out := PublicNode{Name: record.Settings.Name, Region: record.Settings.Region, Provider: record.Settings.Provider, Status: "unknown", MetricStatus: unavailablePublicMetrics()}
	if record.Generation == 0 || record.Session == "" || record.LastSeen.IsZero() || record.LastSeen.After(now) {
		return out
	}
	lastSeen := record.LastSeen
	var snapshot publicMetrics
	state := s.acquireExistingState(record.ID)
	if state != nil {
		state.mu.Lock()
		if state.credential == record.Credential && state.credentialEpoch == record.CredentialEpoch && state.generation == record.Generation && state.session == record.Session {
			lastSeen = record.LatestSeen(state.receivedAt)
			if state.lastSeenPersisted.Equal(record.LastSeen) || state.public.receivedAt.Equal(record.LastSeen) {
				snapshot = state.public
			}
		}
		state.mu.Unlock()
		s.releaseState(state)
	}
	if lastSeen.After(now) {
		return out
	}
	out.LastSeen = new(lastSeen.UTC())
	out.Status = "online"
	if now.Sub(lastSeen) > nodes.OnlineWindow {
		out.Status = "offline"
	}
	snapshot.project(&out, now)
	return out
}

// PublicPage 保持匿名响应的字段顺序与 null 语义。
type PublicPage struct {
	Nodes       []PublicNode `json:"nodes"`
	HasMore     bool         `json:"has_more"`
	NextOffset  *int         `json:"next_offset"`
	GeneratedAt time.Time    `json:"generated_at"`
}

const publicPageCacheLimit = 128

type publicPageKey struct {
	service       *nodes.Service
	limit, offset int
	ttl           time.Duration
}

type publicPageEntry struct {
	page    PublicPage
	version uint64
}

type publicPageCall struct {
	done chan struct{}
	err  error
}

type publicPageCache struct {
	mu      sync.Mutex
	entries map[publicPageKey]publicPageEntry
	calls   map[publicPageKey]*publicPageCall
}

// PublicPage 合并并发的缓存未命中，并限制缓存页数。可见性版本在查询前捕获：
// 竞争的变更无法用新版本为旧数据背书。缓存期间的返回页面不可变。
func (s *Service) PublicPage(ctx context.Context, ns *nodes.Service, limit, offset int, ttl time.Duration, clock func() time.Time) (PublicPage, error) {
	key := publicPageKey{service: ns, limit: limit, offset: offset, ttl: ttl}
	cache := &s.publicPages
	for {
		if err := ctx.Err(); err != nil {
			return PublicPage{}, err
		}
		cache.mu.Lock()
		version := ns.PublicVisibilityVersion()
		now := clock().UTC()
		entry, ok := cache.entries[key]
		if age := now.Sub(entry.page.GeneratedAt); ok && entry.version == version && age >= 0 && age < ttl {
			cache.mu.Unlock()
			return entry.page, nil
		}
		if call := cache.calls[key]; call != nil {
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return PublicPage{}, ctx.Err()
			case <-call.done:
				// 发起共享查询的请求被取消，不代表当前请求也已取消。
				if call.err != nil && !errors.Is(call.err, context.Canceled) && !errors.Is(call.err, context.DeadlineExceeded) {
					return PublicPage{}, call.err
				}
				continue
			}
		}
		if cache.calls == nil {
			cache.calls = make(map[publicPageKey]*publicPageCall)
			cache.entries = make(map[publicPageKey]publicPageEntry)
		}
		call := &publicPageCall{done: make(chan struct{})}
		cache.calls[key] = call
		cache.mu.Unlock()

		records, more, err := ns.ListPublicRecords(ctx, limit, offset)
		page := PublicPage{Nodes: make([]PublicNode, 0, len(records)), HasMore: more, GeneratedAt: now}
		if more {
			next := offset + len(records)
			page.NextOffset = &next
		}
		for _, record := range records {
			page.Nodes = append(page.Nodes, s.ProjectPublicRecord(record, now))
		}
		if err == nil {
			err = ctx.Err()
		}
		cache.mu.Lock()
		if err == nil {
			if len(cache.entries) >= publicPageCacheLimit {
				for old := range cache.entries {
					delete(cache.entries, old)
					break
				}
			}
			cache.entries[key] = publicPageEntry{page: page, version: version}
		}
		call.err = err
		delete(cache.calls, key)
		close(call.done)
		cache.mu.Unlock()
		return page, err
	}
}
