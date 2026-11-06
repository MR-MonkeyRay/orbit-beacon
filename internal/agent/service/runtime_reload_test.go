package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/identity"
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

func TestRuntimeFeatureReloadChangesNextReportAndStopsProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initial := v1.DefaultRuntimeSettings()
		initial.Interfaces = []string{"eth0"}
		initial.ProbeTargets = []string{"tcp://example.test:80"}
		collector, err := collect.New(collect.Options{Config: initial})
		if err != nil {
			t.Fatal(err)
		}
		runtime := &Runtime{settings: initial, collector: collector}
		var checks int
		probes := probe.NewRunner(probe.Options{Check: func(_ context.Context, target string) v1.ProbeResult {
			checks++
			latency := 1.0
			return v1.ProbeResult{Target: target, OK: true, LatencyMS: &latency, CheckedAt: time.Now().UTC()}
		}})
		if err := probes.SetTargets(initial.ProbeTargets); err != nil {
			t.Fatal(err)
		}
		state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		probeDone := make(chan error, 1)
		go func() { probeDone <- probes.Run(ctx) }()
		synctest.Wait()
		traffic := collector.Traffic()
		traffic.Observe(initial.Interfaces, map[string]collect.NetworkCounter{"eth0": {RxBytes: 100, TxBytes: 200}})
		traffic.Observe(initial.Interfaces, map[string]collect.NetworkCounter{"eth0": {RxBytes: 150, TxBytes: 250}})
		outstanding := traffic.Entries()[0]
		reload := make(chan struct{}, 1)
		reports := make(chan v1.Report, 32)
		next := initial
		scheduler := NewScheduler(SchedulerOptions{Identity: state, RuntimeSettings: func() v1.RuntimeSettings { return runtime.settings }, Reload: reload,
			ReloadConfig: func(ctx context.Context) error {
				return runtime.reloadSettings(ctx, probes, func() (v1.RuntimeSettings, error) { return next, nil })
			},
			Collect: func(context.Context) (v1.Metrics, error) {
				canceled, stop := context.WithCancel(ctx)
				stop()
				metrics, _ := collector.Collect(canceled)
				return metrics, nil
			},
			Traffic: collector.Traffic(), Probes: probes,
			Report: func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				if err := report.Validate(); err != nil {
					t.Error(err)
				}
				reports <- report
				return v1.ReportResponse{}, nil
			},
		})
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		synctest.Wait()
		first := <-reports
		if len(first.Metrics.Disks) != 1 || len(first.Metrics.Networks) != 1 || len(first.ProbeResults) != 1 || len(first.Traffic) != 1 || first.Traffic[0] != outstanding {
			t.Fatalf("initial=%+v", first)
		}
		next = initial
		next.DisabledFeatures = v1.DisabledFeatureOrder()
		next.RemoteCommands = []string{}
		next.ProbeTargets = []string{}
		reload <- struct{}{}
		synctest.Wait()
		second := <-reports
		if !second.Runtime.Equal(next) || second.Seq != first.Seq+1 || len(second.Metrics.Disks) != 0 || len(second.Metrics.Networks) != 0 || len(second.Traffic) != 0 || len(second.ProbeResults) != 0 {
			t.Fatalf("disabled=%+v", second)
		}
		before := checks
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if checks != before {
			t.Fatal("disabled probes still running")
		}
		for len(reports) > 0 {
			<-reports
		}
		next = initial
		reload <- struct{}{}
		synctest.Wait()
		last := <-reports
		if !last.Runtime.Equal(initial) || len(last.Metrics.Disks) != 1 || len(last.Metrics.Networks) != 1 || checks <= before || len(last.Traffic) == 0 || last.Traffic[0] != outstanding {
			t.Fatal("re-enable did not restore collection/probes")
		}
		cancel()
		synctest.Wait()
		<-done
		<-probeDone
	})
}

type doctorTransport func(*http.Request) (*http.Response, error)

func (f doctorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDoctorReportsEffectiveCapabilities(t *testing.T) {
	cfg := v1.DefaultRuntimeSettings()
	cfg.RemoteCommands = []string{v1.RemoteCommandRestart}
	cfg.DisabledFeatures = v1.DisabledFeatureOrder()
	collector, err := collect.New(collect.Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{settings: cfg, collector: collector, installMode: "docker", client: &Client{baseURL: "https://example.test", http: &http.Client{Transport: doctorTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodHead {
			t.Fatal("doctor sent non-HEAD request")
		}
		return &http.Response{StatusCode: http.StatusMethodNotAllowed, Header: http.Header{"Allow": []string{http.MethodPost}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}}
	result, err := runtime.Doctor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.RemoteCommands, cfg.RemoteCommands) || !reflect.DeepEqual(result.DisabledFeatures, cfg.DisabledFeatures) {
		t.Fatalf("doctor capabilities=%+v", result)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["remote_commands"]) != `["restart"]` || string(fields["disabled_features"]) != `["disk","network","traffic","probes","disk_io","pressure","temperature"]` {
		t.Fatalf("doctor JSON=%s", body)
	}
}
