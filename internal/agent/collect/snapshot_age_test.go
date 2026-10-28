package collect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestSnapshotSourceAgesUseOneFinalBoundary(t *testing.T) {
	path := t.TempDir()
	config := v1.RuntimeSettings{ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, DiskPaths: []string{path}, Interfaces: []string{"lo"}}
	collector, err := New(Options{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	collector.diskUsage = func(context.Context, string) (*disk.UsageStat, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return &disk.UsageStat{Total: 100, Used: 20}, nil
	}
	result := make(chan v1.Metrics, 1)
	go func() {
		metrics, _ := collector.Collect(context.Background())
		result <- metrics
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("disk observation did not start")
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	var metrics v1.Metrics
	select {
	case metrics = <-result:
	case <-time.After(time.Second):
		t.Fatal("collection did not finish after disk observation")
	}
	if err := metrics.Validate(); err != nil {
		t.Fatal(err)
	}
	loadAge := metrics.Sources["load"].AgeMS
	diskAge := metrics.Sources["disk:"+path].AgeMS
	if loadAge < 50 {
		t.Fatalf("load source age was measured before the final snapshot boundary: %dms", loadAge)
	}
	if diskAge >= loadAge {
		t.Fatalf("disk observation age did not remain newer than earlier load observation: disk=%dms load=%dms", diskAge, loadAge)
	}
}
