package collect

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/sensors"
	v1 "orbitbeacon/internal/protocol/v1"
)

type temperatureCollection struct {
	epoch uint64
	done  chan struct{}
	value *float64
}

func roundMetric(value float64) float64 { return math.Round(value*100) / 100 }
func boundedMetric(value, limit float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= limit
}

// Only whole physical devices have a device entry directly under /sys/block.
func physicalDevice(root, name string) bool {
	if name == "" || filepath.Base(name) != name {
		return false
	}
	_, err := os.Stat(filepath.Join(root, name, "device"))
	return err == nil
}

func (c *Collector) collectDiskIO(ctx context.Context, m *v1.Metrics) {
	counters, err := c.diskIOReader(ctx)
	at := c.hostNow()
	if err != nil || ctx.Err() != nil {
		c.diskIOPrevious = nil
		return
	}
	current := make(map[string]disk.IOCountersStat)
	for name, counter := range counters {
		if physicalDevice(c.sysBlockRoot, name) {
			current[name] = counter
		}
	}
	previous, elapsed := c.diskIOPrevious, at.Sub(c.diskIOAt).Seconds()
	c.diskIOPrevious, c.diskIOAt = current, at
	if len(current) == 0 || len(current) != len(previous) || elapsed <= 0 {
		return
	}
	var read, write, busy float64
	for name, counter := range current {
		old, ok := previous[name]
		if !ok || counter.ReadBytes < old.ReadBytes || counter.WriteBytes < old.WriteBytes || counter.IoTime < old.IoTime {
			return
		}
		read += float64(counter.ReadBytes-old.ReadBytes) / elapsed
		write += float64(counter.WriteBytes-old.WriteBytes) / elapsed
		busy = math.Max(busy, float64(counter.IoTime-old.IoTime)/(elapsed*10))
	}
	if !boundedMetric(read, 1e15) || !boundedMetric(write, 1e15) {
		return
	}
	m.DiskIO = &v1.DiskIOMetrics{ReadBytesPerSec: roundMetric(read), WriteBytesPerSec: roundMetric(write), BusyPercent: roundMetric(math.Min(100, busy))}
}

func parsePressure(content string) *float64 {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if raw, ok := strings.CutPrefix(field, "avg60="); ok {
				value, err := strconv.ParseFloat(raw, 64)
				if err != nil || !boundedMetric(value, 100) {
					return nil
				}
				value = roundMetric(value)
				return &value
			}
		}
	}
	return nil
}

func (c *Collector) collectPressure(ctx context.Context, m *v1.Metrics) {
	pressure := &v1.PressureMetrics{}
	for _, item := range []struct {
		name  string
		value **float64
	}{{"cpu", &pressure.CPU}, {"memory", &pressure.Memory}, {"io", &pressure.IO}} {
		if ctx.Err() != nil {
			return
		}
		content, err := c.pressureReadFile(filepath.Join(c.pressureRoot, item.name))
		if err == nil {
			*item.value = parsePressure(string(content))
		}
	}
	if pressure.CPU != nil || pressure.Memory != nil || pressure.IO != nil {
		m.Pressure = pressure
	}
}

func hottestTemperature(readings []sensors.TemperatureStat, err error) *float64 {
	var warnings *sensors.Warnings
	if err != nil && !errors.As(err, &warnings) {
		return nil
	}
	var hottest *float64
	for _, reading := range readings {
		value := reading.Temperature
		if value > 0 && boundedMetric(value, 150) && (hottest == nil || value > *hottest) {
			v := value
			hottest = &v
		}
	}
	if hottest != nil {
		*hottest = math.Max(0.01, roundMetric(*hottest))
	}
	return hottest
}

func (c *Collector) collectTemperature(ctx context.Context, m *v1.Metrics) {
	now := c.hostNow()
	if job := c.temperaturePending; job != nil {
		select {
		case <-job.done:
			// A failed read keeps the cached value until it expires.
			if job.epoch == c.temperatureEpoch && job.value != nil {
				c.temperatureValue, c.temperatureAt = job.value, c.temperatureStarted
			}
			c.temperaturePending = nil
		default:
		}
	}
	if c.temperaturePending == nil && ctx.Err() == nil && (c.temperatureStarted.IsZero() || now.Sub(c.temperatureStarted) >= time.Minute) {
		job := &temperatureCollection{epoch: c.temperatureEpoch, done: make(chan struct{})}
		c.temperaturePending, c.temperatureStarted = job, now
		read := c.temperatureReader
		// The task owns its result until done closes. A blocked sensor never holds up a report.
		// Keep the snapshot's deadline while allowing fast reports to return before sensors finish.
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(collectionTimeout)
		}
		readCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		go func() {
			defer close(job.done)
			defer cancel()
			readings, err := read(readCtx)
			if readCtx.Err() == nil {
				job.value = hottestTemperature(readings, err)
			}
		}()
	}
	if c.temperatureValue != nil && now.Sub(c.temperatureAt) <= 3*time.Minute {
		m.TemperatureCelsius = c.temperatureValue
	}
}

func (c *Collector) collectHostMetrics(ctx context.Context, m *v1.Metrics) {
	if runtime.GOOS != "linux" {
		return
	}
	if !c.config.FeatureDisabled(v1.FeatureDiskIO) && ctx.Err() == nil {
		c.collectDiskIO(ctx, m)
	}
	if !c.config.FeatureDisabled(v1.FeaturePressure) {
		c.collectPressure(ctx, m)
	}
	if !c.config.FeatureDisabled(v1.FeatureTemperature) {
		c.collectTemperature(ctx, m)
	}
}
