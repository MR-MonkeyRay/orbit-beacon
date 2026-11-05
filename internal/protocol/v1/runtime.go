package v1

import (
	"encoding/json"
	"errors"
	"slices"
)

const (
	RemoteCommandUpgrade = "upgrade"
	RemoteCommandRestart = "restart"
	RemoteCommandStop    = "stop"
	FeatureDisk          = "disk"
	FeatureNetwork       = "network"
	FeatureTraffic       = "traffic"
	FeatureProbes        = "probes"
)

// RemoteCommandOrder 返回远程指令的规范顺序，调用方可安全修改返回值。
func RemoteCommandOrder() []string {
	return []string{RemoteCommandUpgrade, RemoteCommandRestart, RemoteCommandStop}
}

// DisabledFeatureOrder 返回可关闭功能的规范顺序。
func DisabledFeatureOrder() []string {
	return []string{FeatureDisk, FeatureNetwork, FeatureTraffic, FeatureProbes}
}

// RuntimeSettings 是由 Agent 管理的采集与上报配置。
// 默认值仅在读取本地配置时补齐，不用于补齐收到的上报。
type RuntimeSettings struct {
	ReportIntervalSeconds int      `json:"report_interval_seconds"`
	DiskIntervalSeconds   int      `json:"disk_interval_seconds"`
	Interfaces            []string `json:"interfaces"`
	DiskPaths             []string `json:"disk_paths"`
	ProbeTargets          []string `json:"probe_targets"`
	RemoteCommands        []string `json:"remote_commands"`
	DisabledFeatures      []string `json:"disabled_features"`
}

func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{ReportIntervalSeconds: 10, DiskIntervalSeconds: 60,
		Interfaces: []string{}, DiskPaths: []string{}, ProbeTargets: []string{}, RemoteCommands: RemoteCommandOrder(), DisabledFeatures: []string{}}
}

// Equal 比较完整的生效配置；空切片与 nil 的内容相等。
func (c RuntimeSettings) Equal(other RuntimeSettings) bool {
	return c.ReportIntervalSeconds == other.ReportIntervalSeconds && c.DiskIntervalSeconds == other.DiskIntervalSeconds &&
		slices.Equal(c.Interfaces, other.Interfaces) && slices.Equal(c.DiskPaths, other.DiskPaths) &&
		slices.Equal(c.ProbeTargets, other.ProbeTargets) && slices.Equal(c.RemoteCommands, other.RemoteCommands) &&
		slices.Equal(c.DisabledFeatures, other.DisabledFeatures)
}

// Normalized 把缺失的列表写成空数组，让存储和接口始终看到同一种编码。
func (c RuntimeSettings) Normalized() RuntimeSettings {
	for _, list := range []*[]string{&c.Interfaces, &c.DiskPaths, &c.ProbeTargets, &c.RemoteCommands, &c.DisabledFeatures} {
		if *list == nil {
			*list = []string{}
		}
	}
	return c
}

// AllowsCommand 供 Agent 执行指令前检查本地许可；未知指令始终拒绝。
func (c RuntimeSettings) AllowsCommand(name string) bool {
	return slices.Contains(RemoteCommandOrder(), name) && slices.Contains(c.RemoteCommands, name)
}

// FeatureDisabled 同时保护直接使用采集器的调用方：关闭网络也关闭流量。
func (c RuntimeSettings) FeatureDisabled(name string) bool {
	return slices.Contains(c.DisabledFeatures, name) || name == FeatureTraffic && slices.Contains(c.DisabledFeatures, FeatureNetwork)
}

type ValidationError struct {
	Fields       map[string]string
	reportBudget bool
}

func (err *ValidationError) Error() string { return "runtime settings validation failed" }
func (err *ValidationError) Is(target error) bool {
	return target == ErrReportBudget && err.reportBudget
}

func (c RuntimeSettings) Validate() error {
	fields := make(map[string]string)
	if c.ReportIntervalSeconds < 5 || c.ReportIntervalSeconds > 60 {
		fields["report_interval_seconds"] = "must be between 5 and 60"
	}
	if c.DiskIntervalSeconds < 60 || c.DiskIntervalSeconds > 3600 {
		fields["disk_interval_seconds"] = "must be between 60 and 3600"
	}
	for _, selection := range []struct {
		field           string
		values, allowed []string
	}{
		{"remote_commands", c.RemoteCommands, RemoteCommandOrder()},
		{"disabled_features", c.DisabledFeatures, DisabledFeatureOrder()},
	} {
		for i, value := range selection.values {
			if !slices.Contains(selection.allowed, value) {
				fields[selection.field] = "unknown entry"
				break
			}
			if slices.Contains(selection.values[:i], value) {
				fields[selection.field] = "entries must be unique"
				break
			}
		}
	}
	budget := false
	budgetFields := 0
	if err := c.ValidateSelection(); err != nil {
		validation := err.(*ValidationError)
		budget, budgetFields = errors.Is(err, ErrReportBudget), len(validation.Fields)
		for field, message := range validation.Fields {
			fields[field] = message
		}
	}
	if len(c.ProbeTargets) > 5 {
		fields["probe_targets"] = "must contain at most 5 entries"
	} else {
		for i, target := range c.ProbeTargets {
			if err := ValidateProbeTarget(target); err != nil {
				fields["probe_targets"] = err.Error()
				break
			}
			for _, previous := range c.ProbeTargets[:i] {
				if previous == target {
					fields["probe_targets"] = "entries must be unique"
					break
				}
			}
		}
	}
	if len(fields) != 0 {
		return &ValidationError{Fields: fields, reportBudget: budget && len(fields) == budgetFields}
	}
	return nil
}

// ValidateSelection 在变更采集器或探测配置前，校验设备选择范围与上报编码大小。
func (c RuntimeSettings) ValidateSelection() error {
	var fields map[string]string
	for _, selection := range [...]struct {
		field  string
		values []string
		valid  func(string) bool
		limit  string
	}{
		{"disk_paths", c.DiskPaths, validDiskPath, "entries must be absolute paths of at most 1024 bytes without NUL"},
		{"interfaces", c.Interfaces, validInterfaceName, "entries must be nonempty and at most 255 bytes without NUL, CR or LF"},
	} {
		message := ""
		if len(selection.values) > 8 {
			message = "must contain at most 8 entries"
		} else {
			for i, value := range selection.values {
				if !selection.valid(value) {
					message = selection.limit
					break
				}
				for _, previous := range selection.values[:i] {
					if value == previous {
						message = "entries must be unique"
						break
					}
				}
				if message != "" {
					break
				}
			}
		}
		if message != "" {
			if fields == nil {
				fields = make(map[string]string)
			}
			fields[selection.field] = message
		}
	}
	if len(fields) == 0 && !selectionFitsReport(c) {
		fields = make(map[string]string, 3)
		for _, selected := range []struct {
			name  string
			count int
		}{
			{"disk_paths", len(c.DiskPaths)}, {"interfaces", len(c.Interfaces)}, {"probe_targets", len(c.ProbeTargets)},
		} {
			if selected.count > 0 {
				fields[selected.name] = "combined JSON-escaped selections exceed the report size budget"
			}
		}
		return &ValidationError{Fields: fields, reportBudget: true}
	}
	if len(fields) != 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// DecodeRuntimeSettings 解码并校验观测到的运行时配置。
func DecodeRuntimeSettings(raw string) (RuntimeSettings, error) {
	var runtime RuntimeSettings
	if err := json.Unmarshal([]byte(raw), &runtime); err != nil {
		return runtime, err
	}
	return runtime, runtime.Validate()
}
