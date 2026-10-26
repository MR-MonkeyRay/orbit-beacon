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
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"

	v1 "orbitbeacon/internal/protocol/v1"
)

const collectionTimeout = 3 * time.Second

type Options struct {
	Config v1.DesiredConfig
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
	config        v1.DesiredConfig
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
}

func New(options Options) (*Collector, error) {
	now := time.Now()
	c := &Collector{started: now, epoch: strconv.FormatInt(now.UnixNano(), 36), diskUsage: disk.UsageWithContext}
	if err := c.SetConfig(options.Config); err != nil {
		return nil, err
	}
	return c, nil
}

// SetConfig changes selection atomically. Missing devices are collection
// failures, not configuration failures, so other sources can keep working.
func (c *Collector) SetConfig(config v1.DesiredConfig) error {
	if config.ReportIntervalSeconds == 0 {
		config.ReportIntervalSeconds = 10
	}
	if config.DiskIntervalSeconds == 0 {
		config.DiskIntervalSeconds = 60
	}
	if config.ReportIntervalSeconds < 5 || config.ReportIntervalSeconds > 60 || config.DiskIntervalSeconds < 60 || config.DiskIntervalSeconds > 3600 {
		return errors.New("collection interval outside supported bounds")
	}
	if err := config.ValidateSelection(); err != nil {
		return err
	}
	// Preserve owned selections when unchanged; the runtime may pass the same
	// configuration before every sample.
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
	return v1.System{Hostname: name, OS: runtime.GOOS, Arch: runtime.GOARCH}, nil
}

// Collect returns independent failures as null values with explicit statuses.
// Cancellation retains the complete snapshot, including configured series.
func (c *Collector) Collect(parent context.Context) (v1.Metrics, error) {
	ctx, cancel := context.WithTimeout(parent, collectionTimeout)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := v1.Metrics{
		Disks:        make([]v1.DiskMetrics, 0, 8),
		Networks:     make([]v1.NetworkMetrics, 0, 8),
		MetricStatus: make(map[string]string, 26),
		Sources:      make(map[string]v1.SourceMetadata, 21),
	}
	c.collectCPU(ctx, &m)
	c.collectLoad(ctx, &m)
	c.collectMemory(ctx, &m)
	c.collectUptime(ctx, &m)
	c.collectNetworks(ctx, &m)
	c.collectDisks(ctx, &m)
	return m, ctx.Err()
}

func (c *Collector) source(key string, at time.Time, interval time.Duration) v1.SourceMetadata {
	c.sequence++
	kind, _, _ := strings.Cut(key, ":")
	source := v1.SourceMetadata{
		SampleID:         kind + "-" + c.epoch + "-" + strconv.FormatUint(c.sequence, 36),
		AgeMS:            max(int64(0), time.Since(at).Milliseconds()),
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
	source := c.source("cpu", at, c.baseInterval())
	window := int64(0)
	source.MeasurementWindowMS = &window
	m.MetricStatus["cpu_percent"] = "stale"
	if err != nil || len(times) != 1 {
		c.cpuReady = false
		m.MetricStatus["cpu_percent"] = failureStatus(err)
	} else {
		current := times[0]
		if c.cpuReady {
			window = at.Sub(c.cpuAt).Milliseconds()
			total, busy := cpuTimes(current)
			previousTotal, previousBusy := cpuTimes(c.cpuPrevious)
			if window > 0 && total > previousTotal && busy >= previousBusy {
				percent := math.Max(0, math.Min(100, 100*(busy-previousBusy)/(total-previousTotal)))
				if !math.IsNaN(percent) && !math.IsInf(percent, 0) {
					m.CPUPercent = &percent
					m.MetricStatus["cpu_percent"] = "ok"
				}
			}
		}
		c.cpuPrevious, c.cpuAt, c.cpuReady = current, at, true
	}
	m.Sources["cpu"] = source
}

func cpuTimes(t cpu.TimesStat) (total, busy float64) {
	// Linux guest times are already included in User/Nice; do not double count.
	total = t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
	if runtime.GOOS != "linux" {
		total += t.Guest + t.GuestNice
	}
	return total, total - t.Idle - t.Iowait
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
	for _, key := range []string{"load1", "load5", "load15"} {
		m.MetricStatus[key] = status
	}
	m.Sources["load"] = c.source("load", time.Now(), c.baseInterval())
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
	m.MetricStatus["memory_total_bytes"], m.MetricStatus["memory_available_bytes"] = status, status
	m.Sources["memory"] = c.source("memory", time.Now(), c.baseInterval())
	var swap *mem.SwapMemoryStat
	err = checked(ctx, func() (err error) { swap, err = mem.SwapMemoryWithContext(ctx); return err })
	status = "ok"
	if err != nil || swap == nil || swap.Used > swap.Total {
		status = failureStatus(err)
	} else {
		m.SwapTotalBytes, m.SwapUsedBytes = &swap.Total, &swap.Used
	}
	m.MetricStatus["swap_total_bytes"], m.MetricStatus["swap_used_bytes"] = status, status
	m.Sources["swap"] = c.source("swap", time.Now(), c.baseInterval())
}

func (c *Collector) collectUptime(ctx context.Context, m *v1.Metrics) {
	var uptime uint64
	err := checked(ctx, func() (err error) {
		if err = uptimeSourceAvailable(); err != nil {
			return err
		}
		uptime, err = host.UptimeWithContext(ctx)
		return err
	})
	m.MetricStatus["uptime_seconds"] = "ok"
	if err != nil {
		m.MetricStatus["uptime_seconds"] = failureStatus(err)
	} else {
		seconds := float64(uptime)
		m.UptimeSeconds = &seconds
	}
	m.Sources["uptime"] = c.source("uptime", time.Now(), c.baseInterval())
}

func (c *Collector) collectNetworks(ctx context.Context, m *v1.Metrics) {
	names := c.config.Interfaces
	if len(names) == 0 {
		name, err := defaultInterface(ctx)
		if err != nil {
			delete(c.rates.previous, c.autoInterface)
			c.autoInterface = ""
			c.networks = nil
			m.Networks = append(m.Networks, v1.NetworkMetrics{Name: "default"})
			m.MetricStatus["network:default"] = failureStatus(err)
			m.Sources["network:default"] = c.source("network:default", time.Now(), c.networkInterval())
			return
		}
		if c.autoInterface != name {
			delete(c.rates.previous, c.autoInterface)
			c.autoInterface = name
			c.networks = nil
		}
		names = []string{name}
	}
	// gopsutil Darwin invokes system netstat (and sometimes ifconfig).
	// Cache its real observations for 30 seconds; never relabel a replay as new.
	due := runtime.GOOS != "darwin" || c.networkAt.IsZero() || time.Since(c.networkAt) >= c.networkInterval()
	if due {
		var counters []gnet.IOCountersStat
		err := checked(ctx, func() (err error) { counters, err = gnet.IOCountersWithContext(ctx, true); return err })
		at := time.Now()
		c.networkAt = at
		c.networks = make(map[string]networkSample, len(names))
		for _, name := range names {
			observation := NetworkObservation{Network: v1.NetworkMetrics{Name: name}, Status: "unsupported", Source: c.source("network:"+name, at, c.networkInterval())}
			if err != nil {
				observation.Status = failureStatus(err)
				delete(c.rates.previous, name)
			} else {
				found := false
				for _, counter := range counters {
					if counter.Name == name {
						observation = c.rates.Observe(name, NetworkCounter{RxBytes: counter.BytesRecv, TxBytes: counter.BytesSent}, at.Sub(c.started))
						found = true
						break
					}
				}
				if !found {
					delete(c.rates.previous, name)
				}
			}
			c.networks[name] = networkSample{observation: observation, at: at}
		}
	}
	for _, name := range names {
		sample, exists := c.networks[name]
		if !exists {
			// A changed automatic route needs its own baseline, never the old
			// interface's cached rate.
			m.Networks = append(m.Networks, v1.NetworkMetrics{Name: name})
			m.MetricStatus["network:"+name] = "stale"
			m.Sources["network:"+name] = c.source("network:"+name, time.Now(), c.networkInterval())
			continue
		}
		observation := sample.observation
		observation.Source.AgeMS = max(int64(0), time.Since(sample.at).Milliseconds())
		if !observation.Source.Fresh() {
			observation.Status = "stale"
			observation.Network.RxBytesPerSec, observation.Network.TxBytesPerSec = nil, nil
		}
		key := "network:" + name
		m.Networks = append(m.Networks, observation.Network)
		m.MetricStatus[key], m.Sources[key] = observation.Status, observation.Source
	}
}

func noDefaultInterface() error {
	return errors.New("default route interface unavailable; configure an interface explicitly")
}
