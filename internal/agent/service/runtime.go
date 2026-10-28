package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/probe"
	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

// Runtime wires the durable identity, local settings, collector and client.
// Enrollment tokens are never part of runtime state.
type Runtime struct {
	identity    *identity.Store
	settings    v1.RuntimeSettings
	collector   *collect.Collector
	client      *Client
	system      v1.System
	installMode string
}

func OpenRuntime(ctx context.Context, cfg config.Agent) (*Runtime, error) {
	state, err := identity.Open(ctx, filepath.Join(cfg.StateDir, "identity.json"), identity.Options{})
	if err != nil {
		return nil, err
	}
	if err := cfg.RuntimeSettings.Validate(); err != nil {
		return nil, err
	}
	initial := cloneRuntimeSettings(cfg.RuntimeSettings, v1.RuntimeSettings{})
	collector, err := collect.New(collect.Options{Config: initial})
	if err != nil {
		return nil, err
	}
	systemCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	system, err := collect.System(systemCtx)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(ClientOptions{ServerURL: cfg.ServerURL, CAFile: cfg.CAFile, Identity: state, System: system})
	if err != nil {
		return nil, err
	}
	return &Runtime{identity: state, settings: initial, collector: collector, client: client, system: system, installMode: cfg.InstallMode}, nil
}

func (r *Runtime) Close() { r.client.Close() }

func (r *Runtime) Enroll(ctx context.Context, token string) (v1.EnrollmentResponse, error) {
	return r.client.Enroll(ctx, token)
}

func sameRuntimeSettings(a, b v1.RuntimeSettings) bool {
	return a.ReportIntervalSeconds == b.ReportIntervalSeconds && a.DiskIntervalSeconds == b.DiskIntervalSeconds && slices.Equal(a.Interfaces, b.Interfaces) && slices.Equal(a.DiskPaths, b.DiskPaths) && slices.Equal(a.ProbeTargets, b.ProbeTargets)
}

// Arrays belong to an immutable settings snapshot and are copied only on change.
func cloneRuntimeSettings(next, current v1.RuntimeSettings) v1.RuntimeSettings {
	if slices.Equal(next.Interfaces, current.Interfaces) && current.Interfaces != nil {
		next.Interfaces = current.Interfaces
	} else {
		next.Interfaces = append([]string{}, next.Interfaces...)
	}
	if slices.Equal(next.DiskPaths, current.DiskPaths) && current.DiskPaths != nil {
		next.DiskPaths = current.DiskPaths
	} else {
		next.DiskPaths = append([]string{}, next.DiskPaths...)
	}
	if slices.Equal(next.ProbeTargets, current.ProbeTargets) && current.ProbeTargets != nil {
		next.ProbeTargets = current.ProbeTargets
	} else {
		next.ProbeTargets = append([]string{}, next.ProbeTargets...)
	}
	return next
}

func (r *Runtime) Run(ctx context.Context, reload <-chan struct{}, loadSettings func() (v1.RuntimeSettings, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	probes := probe.NewRunner(probe.Options{})
	if err := probes.SetTargets(r.settings.ProbeTargets); err != nil {
		cancel()
		return err
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = probes.Run(ctx) }()
	defer func() { cancel(); <-done }()
	scheduler := NewScheduler(SchedulerOptions{Probes: probes, Traffic: r.collector.Traffic(), Identity: r.identity,
		RuntimeSettings: func() v1.RuntimeSettings { return r.settings }, Reload: reload,
		ReloadConfig: func(ctx context.Context) error { return r.reloadSettings(ctx, probes, loadSettings) },
		Collect:      r.collector.Collect, Report: r.client.Report, System: &r.system, CredentialsChanged: func(invalid bool) {
			if invalid {
				fmt.Fprintln(os.Stderr, "orbitbeacon-agent: credentials_invalid; rechecking in 60 seconds")
			} else {
				fmt.Fprintln(os.Stderr, "orbitbeacon-agent: credentials recovered")
			}
		}, Log: func(message string) {
			fmt.Fprintln(os.Stderr, "orbitbeacon-agent:", message)
		}})
	return scheduler.Run(ctx)
}

func (r *Runtime) reloadSettings(ctx context.Context, probes *probe.Runner, loadSettings func() (v1.RuntimeSettings, error)) error {
	if loadSettings == nil {
		return errors.New("configuration loader unavailable")
	}
	next, err := loadSettings()
	if err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sameRuntimeSettings(next, r.settings) {
		return nil
	}
	next = cloneRuntimeSettings(next, r.settings)
	// Full validation precedes either mutation. Both setters accept every
	// validated RuntimeSettings and cannot fail during application.
	if err := r.collector.SetConfig(next); err != nil {
		return err
	}
	if err := probes.SetTargets(next.ProbeTargets); err != nil {
		return err
	}
	r.settings = next
	return nil
}

type Diagnostics struct {
	Configuration  string            `json:"configuration"`
	Identity       string            `json:"identity"`
	Connectivity   string            `json:"connectivity"`
	Authentication string            `json:"authentication"`
	Collection     string            `json:"collection"`
	InstallMode    string            `json:"install_mode"`
	MetricStatus   map[string]string `json:"metric_status"`
}

// Doctor never sends an enrollment/report or advances the durable generation.
// A method-only HTTPS request exercises DNS, trust and endpoint connectivity,
// without claiming to check node authentication or accepting any sample.
func (r *Runtime) Doctor(ctx context.Context) (Diagnostics, error) {
	result := Diagnostics{Configuration: "valid", Identity: "valid", Connectivity: "error", Authentication: "not_checked", Collection: "error", InstallMode: r.installMode}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, r.client.baseURL+"/api/agent/v1/report", nil)
	if err != nil {
		return result, errors.New("cannot construct diagnostic request")
	}
	response, connectionErr := r.client.http.Do(request)
	if response != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodPost {
			connectionErr = errors.New("agent endpoint check failed")
		}
	}
	if connectionErr == nil {
		result.Connectivity = "ok"
	}
	collectionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	metrics, collectionErr := r.collector.Collect(collectionCtx)
	result.MetricStatus = metrics.MetricStatus
	if collectionErr == nil {
		result.Collection = "ok"
		for _, status := range metrics.MetricStatus {
			if status == "error" || status == "permission_denied" {
				result.Collection = "degraded"
			}
		}
	}
	if connectionErr != nil {
		return result, errors.New("TLS, DNS or server connectivity check failed")
	}
	if collectionErr != nil {
		return result, errors.New("host collection check failed")
	}
	return result, nil
}
