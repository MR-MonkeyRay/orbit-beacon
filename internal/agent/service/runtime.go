package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/probe"
	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

// Runtime 串联持久身份、本地配置、采集器与客户端。
// 注册票据绝不属于运行时状态。
type Runtime struct {
	logger      *slog.Logger
	identity    *identity.Store
	settings    v1.RuntimeSettings
	collector   *collect.Collector
	client      *Client
	system      v1.System
	installMode string
}

func OpenRuntime(ctx context.Context, cfg config.Agent, logger *slog.Logger) (*Runtime, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
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
	return &Runtime{logger: logger, identity: state, settings: initial, collector: collector, client: client, system: system, installMode: cfg.InstallMode}, nil
}

func (r *Runtime) Close() { r.client.Close() }

func (r *Runtime) Enroll(ctx context.Context, token string) (v1.EnrollmentResponse, error) {
	return r.client.Enroll(ctx, token)
}

func sameRuntimeSettings(a, b v1.RuntimeSettings) bool {
	return a.ReportIntervalSeconds == b.ReportIntervalSeconds && a.DiskIntervalSeconds == b.DiskIntervalSeconds && slices.Equal(a.Interfaces, b.Interfaces) && slices.Equal(a.DiskPaths, b.DiskPaths) && slices.Equal(a.ProbeTargets, b.ProbeTargets)
}

// 切片属于不可变的配置快照，只在变化时复制。
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
	scheduler := NewScheduler(SchedulerOptions{Logger: r.logger, Probes: probes, Traffic: r.collector.Traffic(), Identity: r.identity,
		RuntimeSettings: func() v1.RuntimeSettings { return r.settings }, Reload: reload,
		ReloadConfig: func(ctx context.Context) error { return r.reloadSettings(ctx, probes, loadSettings) },
		Collect:      r.collector.Collect, Report: r.client.Report, System: &r.system})
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
	// 修改任何组件前先完成完整校验。两个设置方法均接受所有已通过校验的
	// RuntimeSettings，应用过程中不会失败。
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

// Doctor 不发送注册或指标上报请求，也不推进持久化的 RunGeneration。
// 通过 HEAD 请求检查 DNS 和端点连通性；使用 HTTPS 时还检查证书信任，
// 但不验证节点凭据，也不提交采样数据。
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
