//go:build linux

package collect

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/sensors"
	v1 "orbitbeacon/internal/protocol/v1"
)

func hostCollector(t *testing.T, options Options) *Collector {
	t.Helper()
	options.Config = v1.DefaultRuntimeSettings()
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDiskIODeltasAndBaselineResets(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"sda", "sdb"} {
		if err := os.MkdirAll(filepath.Join(root, name, "device"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	counters := map[string]disk.IOCountersStat{"sda": {ReadBytes: 100, WriteBytes: 200, IoTime: 100}, "sdb": {ReadBytes: 100, WriteBytes: 100, IoTime: 100}}
	c := hostCollector(t, Options{SysBlockRoot: root, Now: func() time.Time { return now }, DiskIOReader: func(context.Context) (map[string]disk.IOCountersStat, error) { return counters, nil }})
	sample := func() *v1.DiskIOMetrics {
		m := v1.Metrics{}
		c.collectDiskIO(context.Background(), &m)
		return m.DiskIO
	}
	if sample() != nil {
		t.Fatal("first sample")
	}
	now = now.Add(2 * time.Second)
	counters["sda"] = disk.IOCountersStat{ReadBytes: 500, WriteBytes: 800, IoTime: 600}
	counters["sdb"] = disk.IOCountersStat{ReadBytes: 300, WriteBytes: 300, IoTime: 1100}
	got := sample()
	if got == nil || got.ReadBytesPerSec != 300 || got.WriteBytesPerSec != 400 || got.BusyPercent != 50 {
		t.Fatalf("%+v", got)
	}
	now = now.Add(time.Second)
	counters["sda"] = disk.IOCountersStat{ReadBytes: 1, WriteBytes: 1, IoTime: 1}
	if sample() != nil {
		t.Fatal("counter decrease")
	}
	now = now.Add(time.Second)
	counters["sda"] = disk.IOCountersStat{ReadBytes: 2, WriteBytes: 2, IoTime: 10001}
	if got := sample(); got == nil || got.BusyPercent != 100 {
		t.Fatalf("reset/clamp: %+v", got)
	}
	now = now.Add(time.Second)
	delete(counters, "sdb")
	if sample() != nil {
		t.Fatal("changed set")
	}
	now = now.Add(time.Second)
	if sample() == nil {
		t.Fatal("new baseline")
	}
	if sample() != nil {
		t.Fatal("zero elapsed")
	}
	// Equal-size replacement must also reset.
	now = now.Add(time.Second)
	delete(counters, "sda")
	counters["sdb"] = disk.IOCountersStat{}
	if sample() != nil {
		t.Fatal("same-size changed set")
	}
}

func TestDiskIOPhysicalFilter(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"sda", "nvme0n1"} {
		if err := os.MkdirAll(filepath.Join(root, name, "device"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"sda1", "loop0", "ram0", "dm-0", "md0"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]bool{"sda": true, "nvme0n1": true, "sda1": false, "loop0": false, "ram0": false, "dm-0": false, "md0": false, "../sda": false} {
		if physicalDevice(root, name) != want {
			t.Fatal(name)
		}
	}
	now := time.Now()
	counter := uint64(100)
	c := hostCollector(t, Options{SysBlockRoot: root, Now: func() time.Time { return now }, DiskIOReader: func(context.Context) (map[string]disk.IOCountersStat, error) {
		return map[string]disk.IOCountersStat{"sda": {ReadBytes: counter}, "sda1": {ReadBytes: counter}, "loop0": {ReadBytes: counter}}, nil
	}})
	c.collectDiskIO(context.Background(), &v1.Metrics{})
	counter = 200
	now = now.Add(time.Second)
	m := v1.Metrics{}
	c.collectDiskIO(context.Background(), &m)
	if m.DiskIO == nil || m.DiskIO.ReadBytesPerSec != 100 {
		t.Fatalf("%+v", m.DiskIO)
	}
}

func TestPressureFilesAndParsing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cpu"), []byte("some avg10=8.00 avg60=12.345 avg300=3 total=9\nfull avg60=99"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "io"), []byte("some avg60=broken"), 0600); err != nil {
		t.Fatal(err)
	}
	c := hostCollector(t, Options{PressureRoot: root})
	m := v1.Metrics{}
	c.collectPressure(context.Background(), &m)
	if m.Pressure == nil || m.Pressure.CPU == nil || *m.Pressure.CPU != 12.35 || m.Pressure.Memory != nil || m.Pressure.IO != nil {
		t.Fatalf("%+v", m.Pressure)
	}
	_ = os.Remove(filepath.Join(root, "cpu"))
	m = v1.Metrics{}
	c.collectPressure(context.Background(), &m)
	if m.Pressure != nil {
		t.Fatal(m.Pressure)
	}
	for _, raw := range []string{"full avg60=1", "some avg60=NaN", "some avg60=Inf", "some avg60=-1", "some avg60=101", "some avg10=1"} {
		if parsePressure(raw) != nil {
			t.Fatal(raw)
		}
	}
}

func TestTemperatureFilteringAndWarnings(t *testing.T) {
	readings := []sensors.TemperatureStat{{Temperature: 0}, {Temperature: -1}, {Temperature: 151}, {Temperature: math.NaN()}, {Temperature: math.Inf(1)}, {Temperature: 42.1234}, {Temperature: 100.126}}
	var warnings sensors.Warnings
	warnings.Add(errors.New("one sensor failed"))
	for _, err := range []error{nil, &warnings} {
		got := hottestTemperature(readings, err)
		if got == nil || *got != 100.13 {
			t.Fatal(got)
		}
	}
	if hottestTemperature(readings, errors.New("failed")) != nil || hottestTemperature(nil, nil) != nil {
		t.Fatal("invalid result")
	}
	if got := hottestTemperature([]sensors.TemperatureStat{{Temperature: 0}}, nil); got != nil {
		t.Fatalf("zero reading accepted: %v", *got)
	}
}

func TestTemperatureCacheAndRefresh(t *testing.T) {
	now := time.Now()
	var reads atomic.Int32
	c := hostCollector(t, Options{Now: func() time.Time { return now }, TemperatureReader: func(context.Context) ([]sensors.TemperatureStat, error) {
		reads.Add(1)
		return []sensors.TemperatureStat{{Temperature: 55.555}}, nil
	}})
	sample := func() *float64 {
		m := v1.Metrics{}
		c.collectTemperature(context.Background(), &m)
		return m.TemperatureCelsius
	}
	sample()
	<-c.temperaturePending.done
	if value := sample(); value == nil || *value != 55.56 {
		t.Fatal(value)
	}
	now = now.Add(59 * time.Second)
	sample()
	if reads.Load() != 1 {
		t.Fatal("refresh too early")
	}
	now = now.Add(time.Second)
	sample()
	<-c.temperaturePending.done
	sample()
	if reads.Load() != 2 {
		t.Fatal(reads.Load())
	}
	now = now.Add(3*time.Minute + time.Nanosecond)
	if sample() != nil {
		t.Fatal("expired cache")
	}
	<-c.temperaturePending.done
}

func TestBlockedTemperatureDoesNotDelayCollectOrStartAnotherRead(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var reads atomic.Int32
	now := time.Now()
	c := hostCollector(t, Options{Now: func() time.Time { return now }, TemperatureReader: func(context.Context) ([]sensors.TemperatureStat, error) {
		reads.Add(1)
		close(started)
		<-release
		return nil, nil
	}})
	c.config.DisabledFeatures = []string{v1.FeatureDisk, v1.FeatureNetwork, v1.FeaturePressure}
	before := time.Now()
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(before) > time.Second {
		t.Fatal("sensor delayed report")
	}
	<-started
	now = now.Add(5 * time.Minute)
	for range 3 {
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 1 {
		t.Fatal(reads.Load())
	}
}

func TestHostFeaturesDisabledDoNoReadsAndConstructorDoesNotReadClock(t *testing.T) {
	for _, feature := range []string{v1.FeatureDiskIO, v1.FeatureDisk, v1.FeaturePressure, v1.FeatureTemperature} {
		t.Run(feature, func(t *testing.T) {
			c := hostCollector(t, Options{Now: func() time.Time { t.Fatal("clock read"); return time.Time{} }, DiskIOReader: func(context.Context) (map[string]disk.IOCountersStat, error) {
				t.Fatal("disk IO read")
				return nil, nil
			}, TemperatureReader: func(context.Context) ([]sensors.TemperatureStat, error) { t.Error("temperature read"); return nil, nil }})
			c.pressureReadFile = func(string) ([]byte, error) { t.Fatal("pressure read"); return nil, nil }
			c.config.DisabledFeatures = []string{v1.FeatureDiskIO, v1.FeaturePressure, v1.FeatureTemperature}
			if feature == v1.FeatureDisk {
				c.config.DisabledFeatures = []string{v1.FeatureDisk, v1.FeaturePressure, v1.FeatureTemperature}
			}
			m := v1.Metrics{}
			c.collectHostMetrics(context.Background(), &m)
			if m.DiskIO != nil || m.Pressure != nil || m.TemperatureCelsius != nil {
				t.Fatal(m)
			}
		})
	}
}

func TestTemperatureBackgroundFinishesAfterReportAndRespectsDeadline(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := hostCollector(t, Options{TemperatureReader: func(ctx context.Context) ([]sensors.TemperatureStat, error) {
		close(started)
		<-release
		return []sensors.TemperatureStat{{Temperature: 42}}, nil
	}})
	c.config.DisabledFeatures = []string{v1.FeatureDisk, v1.FeatureNetwork, v1.FeaturePressure}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	close(release)
	<-c.temperaturePending.done
	m := v1.Metrics{}
	c.collectTemperature(context.Background(), &m)
	if m.TemperatureCelsius == nil || *m.TemperatureCelsius != 42 {
		t.Fatal("report cancellation discarded background result")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m = v1.Metrics{}
	c.temperatureStarted = time.Time{}
	c.collectTemperature(ctx, &m)
	if c.temperaturePending != nil {
		t.Fatal("started read after deadline")
	}
}

func TestTemperatureToggleRejectsOldPendingResult(t *testing.T) {
	release := make(chan struct{})
	c := hostCollector(t, Options{TemperatureReader: func(context.Context) ([]sensors.TemperatureStat, error) {
		<-release
		return []sensors.TemperatureStat{{Temperature: 42}}, nil
	}})
	c.collectTemperature(context.Background(), &v1.Metrics{})
	job := c.temperaturePending
	cfg := v1.DefaultRuntimeSettings()
	cfg.DisabledFeatures = []string{v1.FeatureTemperature}
	if err := c.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := c.SetConfig(v1.DefaultRuntimeSettings()); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-job.done
	m := v1.Metrics{}
	c.collectTemperature(context.Background(), &m)
	if m.TemperatureCelsius != nil {
		t.Fatal("disabled sample reused after re-enable")
	}
}

func TestTemperatureFailedReadKeepsCacheUntilExpiry(t *testing.T) {
	now := time.Now()
	var fail atomic.Bool
	c := hostCollector(t, Options{Now: func() time.Time { return now }, TemperatureReader: func(context.Context) ([]sensors.TemperatureStat, error) {
		if fail.Load() {
			return nil, errors.New("sensor failed")
		}
		return []sensors.TemperatureStat{{Temperature: 42}}, nil
	}})
	sample := func() *float64 {
		m := v1.Metrics{}
		c.collectTemperature(context.Background(), &m)
		return m.TemperatureCelsius
	}
	sample()
	<-c.temperaturePending.done
	if sample() == nil {
		t.Fatal("missing first value")
	}
	fail.Store(true)
	now = now.Add(time.Minute)
	sample()
	<-c.temperaturePending.done
	if value := sample(); value == nil || *value != 42 {
		t.Fatalf("failed read dropped the cache: %v", value)
	}
	now = now.Add(2*time.Minute + time.Nanosecond)
	if sample() != nil {
		t.Fatal("cache outlived its limit")
	}
}

func TestTemperatureExpiredDeadlineDiscardsResult(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := hostCollector(t, Options{TemperatureReader: func(ctx context.Context) ([]sensors.TemperatureStat, error) {
		close(started)
		<-ctx.Done()
		close(release)
		return []sensors.TemperatureStat{{Temperature: 42}}, nil
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c.collectTemperature(ctx, &v1.Metrics{})
	<-started
	<-release
	<-c.temperaturePending.done
	m := v1.Metrics{}
	c.collectTemperature(context.Background(), &m)
	if m.TemperatureCelsius != nil {
		t.Fatal("late result accepted")
	}
}

func TestPressureUnsupportedResourceDoesNotHideOthers(t *testing.T) {
	c := hostCollector(t, Options{})
	c.pressureReadFile = func(path string) ([]byte, error) {
		switch filepath.Base(path) {
		case "cpu":
			return nil, syscall.EOPNOTSUPP
		case "memory":
			return []byte("some avg60=3.14159"), nil
		default:
			return nil, os.ErrNotExist
		}
	}
	m := v1.Metrics{}
	c.collectPressure(context.Background(), &m)
	if m.Pressure == nil || m.Pressure.CPU != nil || m.Pressure.IO != nil || m.Pressure.Memory == nil || *m.Pressure.Memory != 3.14 {
		t.Fatalf("%+v", m.Pressure)
	}
}

func TestDiskIOFailureResetsBaseline(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sda", "device"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	failed := false
	read := uint64(100)
	c := hostCollector(t, Options{SysBlockRoot: root, Now: func() time.Time { return now }, DiskIOReader: func(context.Context) (map[string]disk.IOCountersStat, error) {
		if failed {
			return nil, os.ErrPermission
		}
		return map[string]disk.IOCountersStat{"sda": {ReadBytes: read}}, nil
	}})
	sample := func() *v1.DiskIOMetrics {
		m := v1.Metrics{}
		c.collectDiskIO(context.Background(), &m)
		return m.DiskIO
	}
	sample()
	failed = true
	now = now.Add(time.Second)
	if sample() != nil {
		t.Fatal("failed read")
	}
	failed = false
	read = 200
	now = now.Add(time.Second)
	if sample() != nil {
		t.Fatal("failure kept baseline")
	}
	read = 300
	now = now.Add(3 * time.Second)
	value := sample()
	if value == nil || value.ReadBytesPerSec != 33.33 {
		t.Fatalf("rounded rate: %+v", value)
	}
}

func TestHostMetricFailuresLeaveStatusAndReportAlone(t *testing.T) {
	c := hostCollector(t, Options{PressureRoot: t.TempDir(), DiskIOReader: func(context.Context) (map[string]disk.IOCountersStat, error) { return nil, os.ErrPermission }, TemperatureReader: func(context.Context) ([]sensors.TemperatureStat, error) { return nil, os.ErrPermission }})
	c.config.DisabledFeatures = []string{v1.FeatureDisk, v1.FeatureNetwork}
	// Keep disk IO enabled to exercise its failing reader independently of disk capacity.
	c.config.DisabledFeatures = []string{v1.FeatureNetwork}
	c.diskUsage = func(context.Context, string) (*disk.UsageStat, error) { return nil, os.ErrPermission }
	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.DiskIO != nil || m.Pressure != nil || m.TemperatureCelsius != nil {
		t.Fatal("failed host fields populated")
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(m.MetricStatus) != len(v1.MetricStatusFields())+len(m.Disks)+len(m.Networks) || len(m.Sources) != len(v1.SourceFamilies())+len(m.Disks)+len(m.Networks) {
		t.Fatal("optional host metrics changed status contract")
	}
	<-c.temperaturePending.done
}
