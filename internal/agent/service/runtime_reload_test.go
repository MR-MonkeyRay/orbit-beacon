package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/probe"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestRuntimeReloadValidatesBeforeChangingCollectorOrProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initial := v1.DefaultRuntimeSettings()
		initial.Interfaces = []string{"eth0"}
		initial.DiskPaths = []string{"/old"}
		initial.ProbeTargets = []string{"tcp://old.example:80"}
		collector, err := collect.New(collect.Options{Config: initial})
		if err != nil {
			t.Fatal(err)
		}
		runtime := &Runtime{settings: cloneRuntimeSettings(initial, v1.RuntimeSettings{}), collector: collector}
		runner := probe.NewRunner(probe.Options{Check: func(_ context.Context, target string) v1.ProbeResult {
			latency := 1.0
			return v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, CheckedAt: time.Now().UTC()}
		}})
		if err := runner.SetTargets(initial.ProbeTargets); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		traffic := collector.Traffic()
		traffic.Observe(initial.Interfaces, map[string]collect.NetworkCounter{"eth0": {RxBytes: 100, TxBytes: 200}})
		traffic.Observe(initial.Interfaces, map[string]collect.NetworkCounter{"eth0": {RxBytes: 150, TxBytes: 250}})
		before := traffic.Entries()
		invalid := initial
		invalid.Interfaces = []string{"eth1"}
		invalid.DiskPaths = []string{"/new"}
		invalid.ProbeTargets = []string{"invalid-target"}
		if err := runtime.reloadSettings(ctx, runner, func() (v1.RuntimeSettings, error) { return invalid, nil }); err == nil {
			t.Fatal("invalid reload accepted")
		}
		if !reflect.DeepEqual(runtime.settings, initial) || len(runner.Results(initial.ProbeTargets)) != 1 || !reflect.DeepEqual(before, traffic.Entries()) {
			t.Fatal("invalid reload partially changed active state")
		}
		if err := runtime.reloadSettings(ctx, runner, func() (v1.RuntimeSettings, error) { return v1.RuntimeSettings{}, errors.New("malformed JSON") }); err == nil {
			t.Fatal("loader failure accepted")
		}
		canceledBefore, stopBefore := context.WithCancel(ctx)
		stopBefore()
		oldMetrics, _ := collector.Collect(canceledBefore)
		if len(oldMetrics.Disks) != 1 || oldMetrics.Disks[0].Path != "/old" || len(oldMetrics.Networks) != 1 || oldMetrics.Networks[0].Name != "eth0" {
			t.Fatal("invalid reload changed collector selection")
		}
		next := invalid
		next.ReportIntervalSeconds = 5
		next.ProbeTargets = []string{"tcp://new.example:443"}
		if err := runtime.reloadSettings(ctx, runner, func() (v1.RuntimeSettings, error) { return next, nil }); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(runner.Results(initial.ProbeTargets)) != 0 || len(runner.Results(next.ProbeTargets)) != 1 {
			t.Fatal("probe target cutover failed")
		}
		if !reflect.DeepEqual(before, traffic.Entries()) {
			t.Fatal("successful reload reset cumulative traffic")
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		metrics, _ := collector.Collect(canceled)
		if len(metrics.Disks) != 1 || metrics.Disks[0].Path != "/new" || len(metrics.Networks) != 1 || metrics.Networks[0].Name != "eth1" {
			t.Fatalf("collector did not apply complete selection: %+v", metrics)
		}
		next.Interfaces[0] = "mutated"
		next.DiskPaths[0] = "/mutated"
		next.ProbeTargets[0] = "tcp://mutated:80"
		if runtime.settings.Interfaces[0] != "eth1" || runtime.settings.DiskPaths[0] != "/new" || runtime.settings.ProbeTargets[0] != "tcp://new.example:443" {
			t.Fatal("settings snapshot aliases loader arrays")
		}
		pointer := &runtime.settings.Interfaces[0]
		unchanged := cloneRuntimeSettings(runtime.settings, v1.RuntimeSettings{})
		if err := runtime.reloadSettings(ctx, runner, func() (v1.RuntimeSettings, error) { return unchanged, nil }); err != nil {
			t.Fatal(err)
		}
		if pointer != &runtime.settings.Interfaces[0] {
			t.Fatal("unchanged selection allocated a replacement snapshot")
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
