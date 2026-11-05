package collect

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestFeatureReloadClearsSeriesAndTrafficBaseline(t *testing.T) {
	cfg := v1.DefaultRuntimeSettings()
	cfg.Interfaces = []string{"eth0"}
	cfg.DiskPaths = []string{"/"}
	c, err := New(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	a, now := trafficFixture()
	c.traffic = a
	observeTraffic(a, 100, 200)
	observeTraffic(a, 150, 250)
	before := a.Entries()
	disabled := cfg
	disabled.DisabledFeatures = v1.DisabledFeatureOrder()
	c.disks = map[string]diskSample{"/": {}}
	c.networks = map[string]networkSample{"eth0": {}}
	epoch := c.diskEpoch
	if err := c.SetConfig(disabled); err != nil {
		t.Fatal(err)
	}
	if c.disks != nil || c.networks != nil || c.diskEpoch == epoch || len(a.previous) != 0 {
		t.Fatal("reload retained caches or baseline")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	metrics, _ := c.Collect(ctx)
	if len(metrics.Disks) != 0 || len(metrics.Networks) != 0 || metrics.Disks == nil || metrics.Networks == nil {
		t.Fatalf("disabled series=%+v", metrics)
	}
	for key := range metrics.MetricStatus {
		if strings.HasPrefix(key, "disk:") || strings.HasPrefix(key, "network:") {
			t.Fatal("disabled status", key)
		}
	}
	for key := range metrics.Sources {
		if strings.HasPrefix(key, "disk:") || strings.HasPrefix(key, "network:") {
			t.Fatal("disabled source", key)
		}
	}
	report := v1.Report{ProtocolVersion: 1, RunGeneration: 1, SessionID: "features", Seq: 1, SampledAt: time.Now(), Runtime: disabled, Metrics: metrics, Traffic: []v1.Traffic{}}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Entries(), before) {
		t.Fatal("disable lost outstanding totals")
	}
	*now = now.Add(2 * time.Hour)
	if err := c.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	observeTraffic(a, 10000, 20000)
	resumed := a.Entries()
	if resumed[0] != before[0] || resumed[len(resumed)-1].RxBytes != "0" {
		t.Fatalf("disabled interval counted: %+v", resumed)
	}
	a.Acknowledge(before)
	if len(a.Entries()) != 1 {
		t.Fatal("original identity could not settle retained bucket")
	}
	observeTraffic(a, 10010, 20020)
	if got := a.Entries()[0]; got.RxBytes != "10" || got.TxBytes != "20" {
		t.Fatalf("resumed counters=%+v", got)
	}
	enabled, _ := c.Collect(ctx)
	if len(enabled.Disks) != 1 || len(enabled.Networks) != 1 {
		t.Fatalf("re-enabled series=%+v", enabled)
	}
}
