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
	out := PublicNode{Name: record.Settings.Name, Region: record.Settings.Region, Provider: record.Settings.Provider, Status: "unknown"}
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
	out.Status = "offline"
	if now.Sub(lastSeen) > nodes.OnlineWindow {
		return out
	}
	out.Status = "online"
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
