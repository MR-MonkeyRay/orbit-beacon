package collect

import (
	"github.com/shirou/gopsutil/v4/disk"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestDiskInodesFromExistingUsageReading(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total, used uint64
		present     bool
	}{
		{"supported", 100, 25, true}, {"empty", 100, 0, true}, {"unsupported", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Options{Config: config.DefaultRuntimeSettings()})
			if err != nil {
				t.Fatal(err)
			}
			c.disks = make(map[string]diskSample)
			job := &diskCollection{epoch: c.diskEpoch, count: 1}
			job.reads[0] = diskReading{path: "/", at: time.Now(), usage: &disk.UsageStat{Total: 1000, Used: 250, InodesTotal: tc.total, InodesUsed: tc.used}}
			c.finishDisks(job)
			sample := c.disks["/"]
			if sample.status != "ok" {
				t.Fatalf("status=%s", sample.status)
			}
			got := sample.metric
			if (got.InodesTotal != nil) != tc.present || (got.InodesUsed != nil) != tc.present {
				t.Fatalf("metric=%+v", got)
			}
			if tc.present && (*got.InodesTotal != tc.total || *got.InodesUsed != tc.used) {
				t.Fatalf("metric=%+v", got)
			}
		})
	}
}

func TestStaleDiskOmitsInodeCounts(t *testing.T) {
	total, used := uint64(100), uint64(25)
	m := v1.Metrics{Disks: []v1.DiskMetrics{{Path: "/", TotalBytes: &total, UsedBytes: &used, InodesTotal: &total, InodesUsed: &used}}, MetricStatus: map[string]string{"disk:/": "ok"}, Sources: map[string]v1.SourceMetadata{"disk:/": {SampleID: "disk", AgeMS: 180001, SourceIntervalMS: 60000}}}
	c := &Collector{}
	c.finalizeSnapshot(&m, time.Now())
	if m.Disks[0].InodesTotal != nil || m.Disks[0].InodesUsed != nil || m.MetricStatus["disk:/"] != "stale" {
		t.Fatalf("stale inodes=%+v", m.Disks[0])
	}
}
