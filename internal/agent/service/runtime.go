package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

// Runtime wires the same durable identity, configuration, collector and client
// for foreground commands; enrollment tokens are never part of runtime state.
type Runtime struct {
	identity          *identity.Store
	config            *ConfigStore
	collector         *collect.Collector
	client            *Client
	system            v1.System
	installMode       string
	collectorRevision int64
}

func OpenRuntime(cfg config.Agent) (*Runtime, error) {
	state, err := identity.Open(filepath.Join(cfg.StateDir, "identity.json"), identity.Options{})
	if err != nil {
		return nil, err
	}
	settings, err := OpenConfigStore(filepath.Join(cfg.StateDir, "desired.json"), v1.DesiredConfig{
		Revision: 1, ReportIntervalSeconds: 10, DiskIntervalSeconds: 60,
		DiskPaths: []string{}, Interfaces: []string{}, ProbeTargets: []string{}, BillingDay: 1, QuotaBytes: "0",
	})
	if err != nil {
		return nil, err
	}
	initial := settings.Current()
	collector, err := collect.New(collect.Options{Config: initial})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	system, err := collect.System(ctx)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(ClientOptions{ServerURL: cfg.ServerURL, CAFile: cfg.CAFile, Identity: state, Config: settings, System: system})
	if err != nil {
		return nil, err
	}
	return &Runtime{identity: state, config: settings, collector: collector, client: client, system: system, installMode: cfg.InstallMode, collectorRevision: initial.Revision}, nil
}

func (r *Runtime) Close() { r.client.Close() }

func (r *Runtime) Enroll(ctx context.Context, token string) (v1.EnrollmentResponse, error) {
	return r.client.Enroll(ctx, token)
}

func (r *Runtime) collect(ctx context.Context) (v1.Metrics, error) {
	revision, _ := r.config.settings()
	if revision != r.collectorRevision {
		settings := r.config.Current()
		if err := r.collector.SetConfig(settings); err != nil {
			return v1.Metrics{}, err
		}
		r.collectorRevision = settings.Revision
	}
	return r.collector.Collect(ctx)
}

func (r *Runtime) Run(ctx context.Context) error {
	scheduler := NewScheduler(SchedulerOptions{Identity: r.identity, Config: r.config, Collect: r.collect, Report: r.client.Report, System: &r.system, CollectedRevision: func() int64 { return r.collectorRevision }, CredentialsChanged: func(invalid bool) {
		if invalid {
			fmt.Fprintln(os.Stderr, "orbitbeacon-agent: credentials_invalid; rechecking in 60 seconds")
		} else {
			fmt.Fprintln(os.Stderr, "orbitbeacon-agent: credentials recovered")
		}
	}})
	return scheduler.Run(ctx)
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
	metrics, collectionErr := r.collect(collectionCtx)
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
