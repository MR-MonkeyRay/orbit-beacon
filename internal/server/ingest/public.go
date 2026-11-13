package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/services"
	"orbitbeacon/internal/server/storage"
)

// PublicNode 是严格的匿名投影，独立于内部清单。
type PublicNode struct {
	// Group is nil when disabled, a pointer to nil when ungrouped, or an index.
	Group        **int           `json:"group,omitempty"`
	Fields       []string        `json:"-"`
	Country      string          `json:"country"`
	ExpiresOn    *string         `json:"expires_on"`
	Price        *PublicPrice    `json:"price"`
	BillingCycle string          `json:"billing_cycle"`
	AutoRenew    bool            `json:"auto_renew"`
	Bandwidth    string          `json:"bandwidth"`
	IPVersion    PublicIPVersion `json:"ip_version"`
	NetworkRoute string          `json:"network_route"`
	PlanExtra    string          `json:"plan_extra"`
	Traffic      PublicTraffic   `json:"traffic"`

	Availability  availability.Public `json:"availability"`
	Name          string              `json:"name"`
	Region        string              `json:"region"`
	Provider      string              `json:"provider"`
	Status        string              `json:"status"`
	LastSeen      *time.Time          `json:"last_seen"`
	CPUPercent    *float64            `json:"cpu_percent"`
	MemoryPercent *float64            `json:"memory_percent"`
	Load1         *float64            `json:"load1"`
	UptimeSeconds *float64            `json:"uptime_seconds"`
	MetricStatus  PublicMetricStatus  `json:"metric_status"`
}

type PublicPrice struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}
type PublicIPVersion struct {
	IPv4 bool `json:"ipv4"`
	IPv6 bool `json:"ipv6"`
}

// Usage is deliberately omitted: public pages read quota with their existing node query.
type PublicTraffic struct {
	QuotaBytes string `json:"quota_bytes"`
}

// Absent means private; a selected metric or price without data is explicitly null.
func (p PublicNode) MarshalJSON() ([]byte, error) {
	out := map[string]any{"name": p.Name, "status": p.Status, "last_seen": p.LastSeen}
	if p.Group != nil {
		out["group"] = *p.Group
	}
	statuses := map[string]string{}
	for _, field := range p.Fields {
		switch field {
		case "region":
			out["region"] = p.Region
			out["country"] = p.Country
		case "provider":
			out["provider"] = p.Provider
		case "cpu":
			out["cpu_percent"] = p.CPUPercent
			statuses["cpu_percent"] = p.MetricStatus.CPUPercent
		case "memory":
			out["memory_percent"] = p.MemoryPercent
			statuses["memory_percent"] = p.MetricStatus.MemoryPercent
		case "load":
			out["load1"] = p.Load1
			statuses["load1"] = p.MetricStatus.Load1
		case "uptime":
			out["uptime_seconds"] = p.UptimeSeconds
			statuses["uptime_seconds"] = p.MetricStatus.UptimeSeconds
		case "availability":
			out["availability"] = p.Availability
		case "expires_on":
			out["expires_on"] = p.ExpiresOn
		case "price":
			out["price"] = p.Price
		case "billing_cycle":
			out["billing_cycle"] = p.BillingCycle
			out["auto_renew"] = p.AutoRenew
		case "bandwidth":
			out["bandwidth"] = p.Bandwidth
		case "ip_version":
			out["ip_version"] = p.IPVersion
		case "network_route":
			out["network_route"] = p.NetworkRoute
		case "plan_extra":
			out["plan_extra"] = p.PlanExtra
		case "traffic":
			out["traffic"] = p.Traffic
		}
	}
	if len(statuses) > 0 {
		out["metric_status"] = statuses
	}
	return json.Marshal(out)
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
func (s *Service) ProjectPublicRecord(record nodes.PublicRecord, now time.Time) (PublicNode, error) {
	projection, err := availability.Project(record.AvailabilityJSON, now)
	if err != nil {
		return PublicNode{}, err
	}
	region := record.Region
	if region == "" {
		region = record.GeoLocation
	}
	out := PublicNode{Fields: append([]string{}, record.Settings.Fields...), Country: record.Country, ExpiresOn: record.ExpiresOn,
		BillingCycle: record.BillingCycle, AutoRenew: record.AutoRenew, Bandwidth: record.Bandwidth,
		IPVersion: PublicIPVersion{record.IPv4, record.IPv6}, NetworkRoute: record.NetworkRoute, PlanExtra: record.PlanExtra,
		Traffic: PublicTraffic{record.QuotaBytes}, Availability: projection, Name: record.Name, Region: region, Provider: record.Provider, Status: "unknown", MetricStatus: unavailablePublicMetrics()}
	if record.PriceAmount != "" {
		out.Price = &PublicPrice{record.PriceAmount, record.PriceCurrency}
	}
	if record.Generation == 0 || record.Session == "" || record.LastSeen.IsZero() || record.LastSeen.After(now) {
		return out, nil
	}
	lastSeen := record.LastSeen
	var snapshot publicMetrics
	state := s.acquireExistingState(record.ID)
	if state != nil {
		state.mu.Lock()
		if s.db.Ready() && state.storageGeneration != 0 && state.storageGeneration == s.db.Generation() && state.credential == record.Credential && state.credentialEpoch == record.CredentialEpoch && state.generation == record.Generation && state.session == record.Session {
			lastSeen = record.LatestSeen(state.receivedAt)
			if state.lastSeenPersisted.Equal(record.LastSeen) || state.public.receivedAt.Equal(record.LastSeen) {
				snapshot = state.public
			}
		}
		state.mu.Unlock()
		s.releaseState(state)
	}
	if lastSeen.After(now) {
		return out, nil
	}
	out.LastSeen = new(lastSeen.UTC())
	out.Status = "online"
	if now.Sub(lastSeen) > nodes.OnlineWindow {
		out.Status = "offline"
	}
	snapshot.project(&out, now)
	return out, nil
}

// PublicPage 保持匿名响应的字段顺序与 null 语义。
type PublicPage struct {
	NodeGroups    *[]string                `json:"node_groups,omitempty"`
	ServiceGroups *[]string                `json:"service_groups,omitempty"`
	Services      []services.PublicService `json:"services"`
	Nodes         []PublicNode             `json:"nodes"`
	HasMore       bool                     `json:"has_more"`
	NextOffset    *int                     `json:"next_offset"`
	GeneratedAt   time.Time                `json:"generated_at"`
}

const publicPageCacheLimit = 128

type publicPageKey struct {
	service       *nodes.Service
	services      *services.Manager
	limit, offset int
	ttl           time.Duration
	showGroups    bool
}

type publicPageEntry struct {
	page       PublicPage
	version    uint64
	generation uint64
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
func (s *Service) PublicPage(ctx context.Context, ns *nodes.Service, limit, offset int, ttl time.Duration, clock func() time.Time, serviceSources ...*services.Manager) (PublicPage, error) {
	return s.publicPage(ctx, ns, limit, offset, ttl, clock, false, serviceSources...)
}

// PublicPageWithGroups preserves the original ungrouped entry point and cache shape.
func (s *Service) PublicPageWithGroups(ctx context.Context, ns *nodes.Service, limit, offset int, ttl time.Duration, clock func() time.Time, showGroups bool, serviceSources ...*services.Manager) (PublicPage, error) {
	return s.publicPage(ctx, ns, limit, offset, ttl, clock, showGroups, serviceSources...)
}

func (s *Service) publicPage(ctx context.Context, ns *nodes.Service, limit, offset int, ttl time.Duration, clock func() time.Time, showGroups bool, serviceSources ...*services.Manager) (PublicPage, error) {
	lease, leaseErr := ns.Database().Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return PublicPage{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	key := publicPageKey{service: ns, limit: limit, offset: offset, ttl: ttl, showGroups: showGroups}
	if len(serviceSources) > 0 {
		key.services = serviceSources[0]
	}
	cache := &s.publicPages
	for {
		if err := ctx.Err(); err != nil {
			return PublicPage{}, err
		}
		cache.mu.Lock()
		version := ns.PublicVisibilityVersion()
		now := clock().UTC()
		entry, ok := cache.entries[key]
		if age := now.Sub(entry.page.GeneratedAt); ok && entry.version == version && entry.generation == lease.Generation() && age >= 0 && age < ttl {
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

		records, more, nodeGroups, err := ns.ListPublicRecordsWithGroups(ctx, limit, offset, now, showGroups)
		page := PublicPage{Nodes: make([]PublicNode, 0, len(records)), Services: []services.PublicService{}, HasMore: more, GeneratedAt: now}
		if showGroups {
			page.NodeGroups = &nodeGroups
			page.ServiceGroups = new([]string{})
		}
		if err == nil && key.services != nil {
			var serviceGroups []string
			page.Services, serviceGroups, err = key.services.PublicAtWithGroups(ctx, now, showGroups)
			if showGroups {
				page.ServiceGroups = &serviceGroups
			}
		}
		if more {
			next := offset + len(records)
			page.NextOffset = &next
		}
		for _, record := range records {
			node, projectErr := s.ProjectPublicRecord(record, now)
			if projectErr != nil {
				err = projectErr
				break
			}
			if showGroups {
				node.Group = &record.Group
			}
			page.Nodes = append(page.Nodes, node)
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
			cache.entries[key] = publicPageEntry{page: page, version: version, generation: lease.Generation()}
		}
		call.err = err
		delete(cache.calls, key)
		close(call.done)
		cache.mu.Unlock()
		return page, err
	}
}
