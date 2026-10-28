// SPDX-License-Identifier: GPL-3.0-or-later

// Package collect reads a bounded selection of host metrics. The caller owns one
// sampling loop; uncancellable disk reads use at most one in-flight slow worker.
package collect

import (
	"context"
	"errors"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"

	"orbitbeacon/internal/buildinfo"
	v1 "orbitbeacon/internal/protocol/v1"
)

const collectionTimeout = 3 * time.Second

type Options struct {
	Config v1.RuntimeSettings
}

type diskSample struct {
	metric v1.DiskMetrics
	source v1.SourceMetadata
	status string
	at     time.Time
}

type networkSample struct {
	observation NetworkObservation
	at          time.Time
}

// Collector retains only the latest disk/network samples and differential
// baselines. All elapsed times come from Go's monotonic time component.
type Collector struct {
	mu            sync.Mutex
	config        v1.RuntimeSettings
	started       time.Time
	epoch         string
	sequence      uint64
	cpuPrevious   cpu.TimesStat
	cpuAt         time.Time
	cpuReady      bool
	disks         map[string]diskSample
	diskUsage     func(context.Context, string) (*disk.UsageStat, error)
	diskPending   *diskCollection
	diskEpoch     uint64
	networks      map[string]networkSample
	networkAt     time.Time
	autoInterface string
	rates         NetworkDifferencer
	traffic       *TrafficAccumulator
	sourceAt      map[string]time.Time
}

func New(options Options) (*Collector, error) {
	now := time.Now()
	c := &Collector{traffic: NewTrafficAccumulator(nil), started: now, epoch: strconv.FormatInt(now.UnixNano(), 36), diskUsage: disk.UsageWithContext, sourceAt: make(map[string]time.Time, 21)}
	if err := c.SetConfig(options.Config); err != nil {
		return nil, err
	}
	return c, nil
}

// SetConfig changes selection atomically. Missing devices are collection
// failures, not configuration failures, so other sources can keep working.
func (c *Collector) SetConfig(config v1.RuntimeSettings) error {
	if err := config.Validate(); err != nil {
		return err
	}
	// Preserve owned selections when unchanged; the runtime applies settings
	// only at initialization and explicit reload boundaries.
	c.mu.Lock()
	defer c.mu.Unlock()
	if slices.Equal(c.config.DiskPaths, config.DiskPaths) {
		config.DiskPaths = c.config.DiskPaths
	} else {
		config.DiskPaths = slices.Clone(config.DiskPaths)
	}
	if slices.Equal(c.config.Interfaces, config.Interfaces) {
		config.Interfaces = c.config.Interfaces
	} else {
		config.Interfaces = slices.Clone(config.Interfaces)
	}
	if !slices.Equal(c.config.DiskPaths, config.DiskPaths) || c.config.DiskIntervalSeconds != config.DiskIntervalSeconds {
		c.disks = nil
		c.diskEpoch++
	}
	if !slices.Equal(c.config.Interfaces, config.Interfaces) || c.config.ReportIntervalSeconds != config.ReportIntervalSeconds {
		c.networks = nil
		c.networkAt = time.Time{}
		c.rates = NetworkDifferencer{}
		c.autoInterface = ""
	}
	if c.traffic != nil && !slices.Equal(c.config.Interfaces, config.Interfaces) {
		c.traffic.Observe(config.Interfaces, nil)
	}
	c.config = config
	c.rates.interval = c.networkInterval()
	return nil
}

// System intentionally avoids host.Info: that broader API can inspect process
// data and invoke distribution helpers. Enrollment needs only these safe fields.
func System(ctx context.Context) (v1.System, error) {
	if err := ctx.Err(); err != nil {
		return v1.System{}, err
	}
	name, err := os.Hostname()
	if err != nil {
		return v1.System{}, err
	}
	if len(name) > 255 {
		return v1.System{}, errors.New("hostname exceeds protocol limit")
	}
	return v1.System{Hostname: name, OS: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: buildinfo.Version}, nil
}

// Collect returns independent failures as null values with explicit statuses.
// Cancellation retains the complete snapshot, including configured series.
func (c *Collector) Collect(parent context.Context) (v1.Metrics, error) {
	ctx, cancel := context.WithTimeout(parent, collectionTimeout)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sourceAt == nil {
		c.sourceAt = make(map[string]time.Time, 21)
	} else {
		clear(c.sourceAt)
	}
	m := v1.Metrics{
		Disks:        make([]v1.DiskMetrics, 0, 8),
		Networks:     make([]v1.NetworkMetrics, 0, 8),
		MetricStatus: make(map[string]string, len(v1.MetricStatusFields())+16),
		Sources:      make(map[string]v1.SourceMetadata, len(v1.SourceFamilies())+16),
	}
	c.collectCPU(ctx, &m)
	c.collectLoad(ctx, &m)
	c.collectMemory(ctx, &m)
	c.collectUptime(ctx, &m)
	c.collectNetworks(ctx, &m)
	c.collectDisks(ctx, &m)
	c.finalizeSnapshot(&m, time.Now())
	return m, ctx.Err()
}

func (c *Collector) finalizeSnapshot(m *v1.Metrics, snapshotAt time.Time) {
	for key, source := range m.Sources {
		if observedAt, ok := c.sourceAt[key]; ok {
			source.AgeMS = max(int64(0), snapshotAt.Sub(observedAt).Milliseconds())
			m.Sources[key] = source
		}
	}
	for index, disk := range m.Disks {
		key := v1.DiskMetricKey(disk.Path)
		source := m.Sources[key]
		if !source.Fresh() {
			m.MetricStatus[key] = "stale"
			m.Disks[index].TotalBytes, m.Disks[index].UsedBytes = nil, nil
		}
	}
	for index, network := range m.Networks {
		key := v1.NetworkMetricKey(network.Name)
		source := m.Sources[key]
		if !source.Fresh() {
			m.MetricStatus[key] = "stale"
			m.Networks[index].RxBytesPerSec, m.Networks[index].TxBytesPerSec = nil, nil
		}
	}
}

func (c *Collector) source(key string, at time.Time, interval time.Duration) v1.SourceMetadata {
	c.sequence++
	kind, _, _ := strings.Cut(key, ":")
	c.sourceAt[key] = at
	source := v1.SourceMetadata{
		SampleID:         kind + "-" + c.epoch + "-" + strconv.FormatUint(c.sequence, 36),
		AgeMS:            0,
		SourceIntervalMS: interval.Milliseconds(),
	}
	if kind == "network" {
		window := int64(0)
		source.MeasurementWindowMS = &window
	}
	return source
}

func (c *Collector) baseInterval() time.Duration {
	return time.Duration(c.config.ReportIntervalSeconds) * time.Second
}

func (c *Collector) networkInterval() time.Duration {
	if runtime.GOOS == "darwin" {
		return 30 * time.Second
	}
	return c.baseInterval()
}

func failureStatus(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not implemented") {
		return "unsupported"
	}
	return "error"
}

func checked(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *Collector) collectCPU(ctx context.Context, m *v1.Metrics) {
	var times []cpu.TimesStat
	err := checked(ctx, func() (err error) { times, err = cpu.TimesWithContext(ctx, false); return err })
	at := time.Now()
	source := c.source(v1.SourceCPU, at, c.baseInterval())
	window := int64(0)
	source.MeasurementWindowMS = &window
	m.MetricStatus[v1.MetricCPUPercent] = "stale"
	if err != nil || len(times) != 1 {
		c.cpuReady = false
		m.MetricStatus[v1.MetricCPUPercent] = failureStatus(err)
	} else {
		current := times[0]
		if c.cpuReady {
			window = at.Sub(c.cpuAt).Milliseconds()
			total, busy := cpuTimes(current)
			previousTotal, previousBusy := cpuTimes(c.cpuPrevious)
			totalDelta, busyDelta := total-previousTotal, busy-previousBusy
			if window > 0 && cpuCountersContinuous(c.cpuPrevious, current) && totalDelta > 0 && busyDelta >= 0 && busyDelta <= totalDelta {
				percent := 100 * (busyDelta / totalDelta)
				if !math.IsNaN(percent) && !math.IsInf(percent, 0) {
					m.CPUPercent = &percent
					m.MetricStatus[v1.MetricCPUPercent] = "ok"
				}
			}
		}
		c.cpuPrevious, c.cpuAt, c.cpuReady = current, at, true
	}
	m.Sources[v1.SourceCPU] = source
}

func cpuTimes(t cpu.TimesStat) (total, busy float64) {
	// Linux guest times are already included in User/Nice; do not double count.
	total = t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
	if runtime.GOOS != "linux" {
		total += t.Guest + t.GuestNice
	}
	return total, total - t.Idle - t.Iowait
}

func cpuCountersContinuous(previous, current cpu.TimesStat) bool {
	// Linux iowait may decrease on SMP; idle+iowait remains the idle total.
	previousIdle := previous.Idle + previous.Iowait
	currentIdle := current.Idle + current.Iowait
	if !validCPUCounter(previous.Idle) || !validCPUCounter(current.Idle) ||
		!validCPUCounter(previous.Iowait) || !validCPUCounter(current.Iowait) ||
		math.IsInf(previousIdle, 0) || math.IsInf(currentIdle, 0) || currentIdle < previousIdle {
		return false
	}
	for _, counter := range [...][2]float64{
		{previous.User, current.User}, {previous.System, current.System},
		{previous.Nice, current.Nice}, {previous.Irq, current.Irq},
		{previous.Softirq, current.Softirq}, {previous.Steal, current.Steal},
		{previous.Guest, current.Guest}, {previous.GuestNice, current.GuestNice},
	} {
		if !validCPUCounter(counter[0]) || !validCPUCounter(counter[1]) || counter[1] < counter[0] {
			return false
		}
	}
	return true
}

func validCPUCounter(counter float64) bool {
	return counter >= 0 && !math.IsNaN(counter) && !math.IsInf(counter, 0)
}

type loadValues struct {
	one, five, fifteen float64
}

func (c *Collector) collectLoad(ctx context.Context, m *v1.Metrics) {
	var value loadValues
	err := checked(ctx, func() (err error) {
		value, err = loadAverage(ctx)
		return err
	})
	status := "ok"
	if err != nil {
		status = failureStatus(err)
	} else {
		m.Load1, m.Load5, m.Load15 = &value.one, &value.five, &value.fifteen
	}
	for _, key := range []string{v1.MetricLoad1, v1.MetricLoad5, v1.MetricLoad15} {
		m.MetricStatus[key] = status
	}
	m.Sources[v1.SourceLoad] = c.source(v1.SourceLoad, time.Now(), c.baseInterval())
}

func (c *Collector) collectMemory(ctx context.Context, m *v1.Metrics) {
	var memory *mem.VirtualMemoryStat
	err := checked(ctx, func() (err error) { memory, err = mem.VirtualMemoryWithContext(ctx); return err })
	status := "ok"
	if err != nil || memory == nil || memory.Total == 0 || memory.Available > memory.Total {
		status = failureStatus(err)
	} else {
		m.MemoryTotalBytes, m.MemoryAvailableBytes = &memory.Total, &memory.Available
	}
	m.MetricStatus[v1.MetricMemoryTotalBytes], m.MetricStatus[v1.MetricMemoryAvailableBytes] = status, status
	m.Sources[v1.SourceMemory] = c.source(v1.SourceMemory, time.Now(), c.baseInterval())
	var swap *mem.SwapMemoryStat
	err = checked(ctx, func() (err error) { swap, err = mem.SwapMemoryWithContext(ctx); return err })
	status = "ok"
	if err != nil || swap == nil || swap.Used > swap.Total {
		status = failureStatus(err)
	} else {
		m.SwapTotalBytes, m.SwapUsedBytes = &swap.Total, &swap.Used
	}
	m.MetricStatus[v1.MetricSwapTotalBytes], m.MetricStatus[v1.MetricSwapUsedBytes] = status, status
	m.Sources[v1.SourceSwap] = c.source(v1.SourceSwap, time.Now(), c.baseInterval())
}

func (c *Collector) collectUptime(ctx context.Context, m *v1.Metrics) {
	var uptime float64
	err := checked(ctx, func() (err error) {
		uptime, err = uptimeValue(ctx)
		return err
	})
	m.MetricStatus[v1.MetricUptimeSeconds] = "ok"
	if err != nil {
		m.MetricStatus[v1.MetricUptimeSeconds] = failureStatus(err)
	} else {
		m.UptimeSeconds = &uptime
	}
	m.Sources[v1.SourceUptime] = c.source(v1.SourceUptime, time.Now(), c.baseInterval())
}

// Traffic exposes the concurrency-safe cumulative traffic state to reporting.
func (c *Collector) Traffic() *TrafficAccumulator { return c.traffic }

func (c *Collector) collectNetworks(ctx context.Context, m *v1.Metrics) {
	names := c.config.Interfaces
	if len(names) == 0 {
		name, err := defaultInterface(ctx)
		if err != nil {
			c.rates.forget(c.autoInterface)
			if c.traffic != nil {
				c.traffic.Observe(nil, nil)
			}
			c.autoInterface = ""
			c.networks = nil
			m.Networks = append(m.Networks, v1.NetworkMetrics{Name: "default"})
			networkKey := v1.NetworkMetricKey("default")
			m.MetricStatus[networkKey] = failureStatus(err)
			m.Sources[networkKey] = c.source(networkKey, time.Now(), c.networkInterval())
			return
		}
		if c.autoInterface != name {
			c.rates.forget(c.autoInterface)
			c.autoInterface = name
			c.networks = nil
		}
		names = []string{name}
	}
	if c.traffic != nil {
		c.traffic.Observe(names, nil)
	}
	// gopsutil Darwin invokes system netstat (and sometimes ifconfig).
	// Cache its real observations for 30 seconds; never relabel a replay as new.
	due := runtime.GOOS != "darwin" || c.networkAt.IsZero() || time.Since(c.networkAt) >= c.networkInterval()
	if due {
		var counters []gnet.IOCountersStat
		err := checked(ctx, func() (err error) { counters, err = networkCounters(ctx); return err })
		at := time.Now()
		c.networkAt = at
		c.networks = make(map[string]networkSample, len(names))
		trafficCounters := make(map[string]NetworkCounter, len(names))
		for _, name := range names {
			networkKey := v1.NetworkMetricKey(name)
			observation := NetworkObservation{Network: v1.NetworkMetrics{Name: name}, Status: "unsupported", Source: c.source(networkKey, at, c.networkInterval())}
			if err != nil {
				observation.Status = failureStatus(err)
				c.rates.forget(name)
			} else {
				found := false
				for _, counter := range counters {
					if counter.Name == name {
						raw := NetworkCounter{RxBytes: counter.BytesRecv, TxBytes: counter.BytesSent}
						trafficCounters[name] = raw
						observation = c.rates.Observe(name, raw, at.Sub(c.started))
						found = true
						break
					}
				}
				if !found {
					c.rates.forget(name)
				}
			}
			c.networks[name] = networkSample{observation: observation, at: at}
		}
		if c.traffic != nil {
			c.traffic.ObserveAt(at, names, trafficCounters)
		}
	}
	for _, name := range names {
		sample, exists := c.networks[name]
		if !exists {
			// A changed automatic route needs its own baseline, never the old
			// interface's cached rate.
			m.Networks = append(m.Networks, v1.NetworkMetrics{Name: name})
			networkKey := v1.NetworkMetricKey(name)
			m.MetricStatus[networkKey] = "stale"
			m.Sources[networkKey] = c.source(networkKey, time.Now(), c.networkInterval())
			continue
		}
		observation := sample.observation
		key := v1.NetworkMetricKey(name)
		c.sourceAt[key] = sample.at
		m.Networks = append(m.Networks, observation.Network)
		m.MetricStatus[key], m.Sources[key] = observation.Status, observation.Source
	}
}

func noDefaultInterface() error {
	return errors.New("default route interface unavailable; configure an interface explicitly")
}
