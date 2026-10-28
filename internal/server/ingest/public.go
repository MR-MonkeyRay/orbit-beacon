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

// PublicNode is a strict anonymous projection, independent of internal inventory.
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
	if !m.valid || m.status == "stale" || m.ageMS < 0 || m.intervalMS <= 0 {
		return false
	}
	if m.intervalMS > math.MaxInt64/3 {
		return true
	}
	limit := max(3*m.intervalMS, int64(45000))
	return m.ageMS <= limit && elapsedMS <= limit-m.ageMS
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

// PublicNode reads durable identity before taking any ingest state lock.
// It never creates state or decodes raw reports.
func (s *Service) PublicNode(ctx context.Context, nodeID string, now time.Time) (PublicNode, error) {
	record, err := nodes.ReadPublicRecord(ctx, s.db, nodeID)
	if err != nil {
		return PublicNode{}, err
	}
	return s.ProjectPublicRecord(record, now), nil
}

// ProjectPublicRecord overlays reception time only for the same durable identity.
// Metrics are omitted if a durable report committed after the page query.
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

// PublicPage preserves the anonymous response's field order and null semantics.
type PublicPage struct {
	Nodes       []PublicNode `json:"nodes"`
	HasMore     bool         `json:"has_more"`
	NextOffset  *int         `json:"next_offset"`
	GeneratedAt time.Time    `json:"generated_at"`
}

const publicPageTTL = 2 * time.Second
const publicPageCacheLimit = 128

type publicPageKey struct {
	service       *nodes.Service
	limit, offset int
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

// PublicPage coalesces concurrent misses and bounds cached pages. The visibility
// version is captured before querying: a racing mutation cannot bless old data
// with a new version. Returned pages are immutable while cached.
func (s *Service) PublicPage(ctx context.Context, ns *nodes.Service, limit, offset int, clock func() time.Time) (PublicPage, error) {
	key := publicPageKey{service: ns, limit: limit, offset: offset}
	cache := &s.publicPages
	for {
		if err := ctx.Err(); err != nil {
			return PublicPage{}, err
		}
		cache.mu.Lock()
		version := ns.PublicVisibilityVersion()
		now := clock().UTC()
		entry, ok := cache.entries[key]
		if age := now.Sub(entry.page.GeneratedAt); ok && entry.version == version && age >= 0 && age < publicPageTTL {
			cache.mu.Unlock()
			return entry.page, nil
		}
		if call := cache.calls[key]; call != nil {
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return PublicPage{}, ctx.Err()
			case <-call.done:
				// The leader's own cancellation says nothing about this request.
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
