package v1

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ProtocolVersion = 1

var ErrProtocolUnsupported = errors.New("protocol unsupported")

// DecimalUint64 keeps cumulative byte counters exact across JSON consumers.
// Its wire representation is always a canonical, quoted unsigned decimal.
type DecimalUint64 string

func (n DecimalUint64) Validate() error {
	value := string(n)
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return errors.New("counter must be a canonical unsigned decimal")
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return errors.New("counter must be a canonical unsigned decimal")
		}
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return errors.New("counter exceeds uint64 range")
	}
	return nil
}

func (n DecimalUint64) MarshalJSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(string(n))
}

func (n *DecimalUint64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '"' {
		return errors.New("counter must be a decimal JSON string")
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	value := DecimalUint64(text)
	if err := value.Validate(); err != nil {
		return err
	}
	*n = value
	return nil
}

type System struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

type EnrollmentRequest struct {
	ProtocolVersion   int    `json:"protocol_version"`
	InstallationID    string `json:"installation_id"`
	NodeToken         string `json:"node_token"`
	PreviousNodeToken string `json:"previous_node_token,omitempty"`
	System            System `json:"system"`
}

func (r EnrollmentRequest) Validate() error {
	if r.ProtocolVersion != ProtocolVersion {
		return ErrProtocolUnsupported
	}
	if err := validateEnrollmentCredential(r.InstallationID, r.NodeToken); err != nil {
		return err
	}
	if r.PreviousNodeToken != "" && (len(r.PreviousNodeToken) < 32 || len(r.PreviousNodeToken) > 256) {
		return errors.New("invalid previous node credential")
	}
	return r.System.Validate()
}

func (s System) Validate() error {
	if len(s.Hostname) > 255 || len(s.OS) > 64 || len(s.Arch) > 64 {
		return errors.New("system identity exceeds field limits")
	}
	return nil
}

// EnrollmentCheckVersion is independent from the enrollment/report wire version:
// v2 distinguishes an active credential from an unbound initial identity.
const EnrollmentCheckVersion = 2

type EnrollmentCheckRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	InstallationID  string `json:"installation_id"`
	NodeToken       string `json:"node_token"`
}

func (r EnrollmentCheckRequest) Validate() error {
	if r.ProtocolVersion != EnrollmentCheckVersion {
		return ErrProtocolUnsupported
	}
	return validateEnrollmentCredential(r.InstallationID, r.NodeToken)
}

type EnrollmentCheckResponse struct {
	Claimed         bool      `json:"claimed"`
	CredentialState string    `json:"credential_state"`
	NodeID          string    `json:"node_id,omitempty"`
	ServerTime      time.Time `json:"server_time"`
}

func validateEnrollmentCredential(installation, token string) error {
	if strings.TrimSpace(installation) == "" || len(installation) > 255 {
		return errors.New("invalid installation identity")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		decoded, err = hex.DecodeString(token)
	}
	if err != nil || len(decoded) != 32 {
		return errors.New("node credential must contain 256 bits")
	}
	return nil
}

type EnrollmentResponse struct {
	NodeID     string    `json:"node_id"`
	ServerTime time.Time `json:"server_time"`
}

// RecoveryRequest confirms the installation bound to a node credential without
// creating a report, advancing runtime ownership, or refreshing node liveness.
type RecoveryRequest struct {
	InstallationID string `json:"installation_id"`
}

func (r RecoveryRequest) Validate() error {
	if strings.TrimSpace(r.InstallationID) == "" || len(r.InstallationID) > 255 {
		return errors.New("invalid installation identity")
	}
	return nil
}

type SourceMetadata struct {
	SampleID            string `json:"sample_id"`
	AgeMS               int64  `json:"age_ms"`
	SourceIntervalMS    int64  `json:"source_interval_ms"`
	MeasurementWindowMS *int64 `json:"measurement_window_ms,omitempty"`
}

// Source is the short name used by protocol consumers.
type Source = SourceMetadata

func (s SourceMetadata) Validate() error {
	if s.SampleID == "" || len(s.SampleID) > 255 || s.AgeMS < 0 || s.SourceIntervalMS <= 0 {
		return errors.New("invalid metric source")
	}
	if s.MeasurementWindowMS != nil && *s.MeasurementWindowMS < 0 {
		return errors.New("negative measurement window")
	}
	return nil
}

func (s SourceMetadata) Fresh() bool {
	if s.Validate() != nil {
		return false
	}
	if s.SourceIntervalMS > math.MaxInt64/3 {
		return true
	}
	return s.AgeMS <= max(3*s.SourceIntervalMS, 45000)
}

type DiskMetrics struct {
	Path       string    `json:"path"`
	TotalBytes *uint64   `json:"total_bytes"`
	UsedBytes  *uint64   `json:"used_bytes"`
	SampledAt  time.Time `json:"sampled_at"`
}

type NetworkMetrics struct {
	Name          string   `json:"name"`
	RxBytesPerSec *float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec *float64 `json:"tx_bytes_per_sec"`
}

type Metrics struct {
	CPUPercent           *float64                  `json:"cpu_percent"`
	Load1                *float64                  `json:"load1"`
	Load5                *float64                  `json:"load5"`
	Load15               *float64                  `json:"load15"`
	MemoryTotalBytes     *uint64                   `json:"memory_total_bytes"`
	MemoryAvailableBytes *uint64                   `json:"memory_available_bytes"`
	SwapTotalBytes       *uint64                   `json:"swap_total_bytes"`
	SwapUsedBytes        *uint64                   `json:"swap_used_bytes"`
	UptimeSeconds        *float64                  `json:"uptime_seconds"`
	Disks                []DiskMetrics             `json:"disks"`
	Networks             []NetworkMetrics          `json:"networks"`
	MetricStatus         map[string]string         `json:"metric_status"`
	Sources              map[string]SourceMetadata `json:"sources"`
}

func validStatus(status string) bool {
	switch status {
	case "ok", "unsupported", "permission_denied", "disabled", "error", "stale":
		return true
	default:
		return false
	}
}

func (m Metrics) Validate() error {
	if len(m.MetricStatus) != len(metricStatusFields)+len(m.Disks)+len(m.Networks) || len(m.Sources) != len(sourceFamilies)+len(m.Disks)+len(m.Networks) {
		return errors.New("metric statuses and sources must match the reported fields and series exactly")
	}
	checkSource := func(key string) error {
		source, exists := m.Sources[key]
		if !exists {
			return fmt.Errorf("metric source %s is required", key)
		}
		if err := source.Validate(); err != nil {
			return fmt.Errorf("metric source %s: %w", key, err)
		}
		if (key == "cpu" || strings.HasPrefix(key, "network:")) && source.MeasurementWindowMS == nil {
			return fmt.Errorf("metric source %s requires a measurement window", key)
		}
		return nil
	}
	for _, key := range sourceFamilies {
		if err := checkSource(key); err != nil {
			return err
		}
	}
	checkMissing := func(field string, missing bool) error {
		if !validStatus(m.MetricStatus[field]) {
			return fmt.Errorf("invalid or missing metric status for %s", field)
		}
		if missing && m.MetricStatus[field] == "ok" {
			return fmt.Errorf("unavailable metric %s requires a failure status", field)
		}
		return nil
	}
	checkFloat := func(field string, value *float64) error {
		if err := checkMissing(field, value == nil); err != nil {
			return err
		}
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
			return fmt.Errorf("metric %s must be finite and nonnegative", field)
		}
		return nil
	}
	for field, value := range map[string]*float64{
		MetricCPUPercent: m.CPUPercent, MetricLoad1: m.Load1, MetricLoad5: m.Load5,
		MetricLoad15: m.Load15, MetricUptimeSeconds: m.UptimeSeconds,
	} {
		if err := checkFloat(field, value); err != nil {
			return err
		}
	}
	if m.CPUPercent != nil && *m.CPUPercent > 100 {
		return errors.New("cpu percentage exceeds 100")
	}
	for field, value := range map[string]*uint64{
		MetricMemoryTotalBytes: m.MemoryTotalBytes, MetricMemoryAvailableBytes: m.MemoryAvailableBytes,
		MetricSwapTotalBytes: m.SwapTotalBytes, MetricSwapUsedBytes: m.SwapUsedBytes,
	} {
		if err := checkMissing(field, value == nil); err != nil {
			return err
		}
	}
	if m.MemoryTotalBytes != nil && m.MemoryAvailableBytes != nil && *m.MemoryAvailableBytes > *m.MemoryTotalBytes {
		return errors.New("available memory exceeds total")
	}
	if m.SwapTotalBytes != nil && m.SwapUsedBytes != nil && *m.SwapUsedBytes > *m.SwapTotalBytes {
		return errors.New("used swap exceeds total")
	}
	if len(m.Disks) > 8 || len(m.Networks) > 8 {
		return errors.New("too many metric series")
	}
	disks := make(map[string]bool, len(m.Disks))
	for _, disk := range m.Disks {
		if strings.TrimSpace(disk.Path) == "" || len(disk.Path) > 1024 || strings.ContainsRune(disk.Path, 0) || disks[disk.Path] {
			return errors.New("invalid or duplicate disk path")
		}
		disks[disk.Path] = true
		if err := checkSource(DiskMetricKey(disk.Path)); err != nil {
			return err
		}
		if err := checkMissing(DiskMetricKey(disk.Path), disk.TotalBytes == nil || disk.UsedBytes == nil); err != nil {
			return err
		}
		if disk.TotalBytes != nil && disk.UsedBytes != nil && *disk.UsedBytes > *disk.TotalBytes {
			return errors.New("used disk bytes exceed total")
		}
		if disk.TotalBytes != nil && disk.UsedBytes != nil && disk.SampledAt.IsZero() {
			return errors.New("disk sample time is required")
		}
	}
	networks := make(map[string]bool, len(m.Networks))
	for _, network := range m.Networks {
		if !validInterfaceName(network.Name) || networks[network.Name] {
			return errors.New("invalid or duplicate interface")
		}
		networks[network.Name] = true
		if err := checkSource(NetworkMetricKey(network.Name)); err != nil {
			return err
		}
		for _, value := range []*float64{network.RxBytesPerSec, network.TxBytesPerSec} {
			if err := checkFloat(NetworkMetricKey(network.Name), value); err != nil {
				return err
			}
		}
	}
	return nil
}

type Capabilities struct {
	SelfUpdate bool `json:"self_update"`
}

type Traffic struct {
	Epoch       string        `json:"epoch"`
	PeriodStart time.Time     `json:"period_start"`
	RxBytes     DecimalUint64 `json:"rx_bytes"`
	TxBytes     DecimalUint64 `json:"tx_bytes"`
}

func (t Traffic) Validate() error {
	if t.Epoch == "" || len(t.Epoch) > 255 || t.PeriodStart.IsZero() {
		return errors.New("invalid traffic identity")
	}
	if err := t.RxBytes.Validate(); err != nil {
		return err
	}
	return t.TxBytes.Validate()
}

type ProbeResult = json.RawMessage

type UpdateStatus struct {
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	ReleaseSeq uint64 `json:"release_seq"`
}

type UpdateJob struct {
	JobID      string `json:"job_id"`
	ReleaseSeq uint64 `json:"release_seq"`
}

type ConfigError struct {
	Revision int64             `json:"revision"`
	Code     string            `json:"code"`
	Fields   map[string]string `json:"fields,omitempty"`
}

type Report struct {
	ProtocolVersion int           `json:"protocol_version"`
	RunGeneration   uint64        `json:"run_generation"`
	SessionID       string        `json:"session_id"`
	Seq             uint64        `json:"seq"`
	SampledAt       time.Time     `json:"sampled_at"`
	ConfigRevision  int64         `json:"config_revision"`
	System          *System       `json:"system,omitempty"`
	Metrics         Metrics       `json:"metrics"`
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
	Traffic         []Traffic     `json:"traffic"`
	ProbeResults    []ProbeResult `json:"probe_results,omitzero"`
	UpdateStatus    *UpdateStatus `json:"update_status,omitempty"`
	ConfigError     *ConfigError  `json:"config_error,omitempty"`
}

func (r Report) Validate() error {
	if r.ProtocolVersion != ProtocolVersion {
		return ErrProtocolUnsupported
	}
	if r.RunGeneration == 0 || r.Seq == 0 || strings.TrimSpace(r.SessionID) == "" || len(r.SessionID) > 255 || r.SampledAt.IsZero() || r.ConfigRevision < 1 {
		return errors.New("invalid report identity, time or revision")
	}
	if len(r.ProbeResults) != 0 {
		return errors.New("probe results are not supported by this protocol slice")
	}
	if r.System != nil {
		if err := r.System.Validate(); err != nil {
			return err
		}
	}
	if r.ConfigError != nil && (r.ConfigError.Revision < 0 || r.ConfigError.Code == "" || len(r.ConfigError.Code) > 64) {
		return errors.New("invalid configuration error")
	}
	if err := r.Metrics.Validate(); err != nil {
		return err
	}
	for _, traffic := range r.Traffic {
		if err := traffic.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type DesiredConfig struct {
	Revision              int64    `json:"revision"`
	ReportIntervalSeconds int      `json:"report_interval_seconds"`
	DiskIntervalSeconds   int      `json:"disk_interval_seconds"`
	Interfaces            []string `json:"interfaces"`
	DiskPaths             []string `json:"disk_paths"`
	ProbeTargets          []string `json:"probe_targets"`
	BillingDay            int      `json:"billing_day"`
	QuotaBytes            string   `json:"quota_bytes"`
}

type ValidationError struct {
	Fields       map[string]string
	reportBudget bool
}

func (err *ValidationError) Error() string { return "desired configuration validation failed" }

func (err *ValidationError) Is(target error) bool {
	return target == ErrReportBudget && err.reportBudget
}

func (c DesiredConfig) Validate() error {
	fields := make(map[string]string)
	reportBudget := false
	reportBudgetFields := 0
	if c.Revision < 1 {
		fields["revision"] = "must be positive"
	}
	if c.ReportIntervalSeconds < 5 || c.ReportIntervalSeconds > 60 {
		fields["report_interval_seconds"] = "must be between 5 and 60"
	}
	if c.DiskIntervalSeconds < 60 || c.DiskIntervalSeconds > 3600 {
		fields["disk_interval_seconds"] = "must be between 60 and 3600"
	}
	if c.BillingDay < 1 || c.BillingDay > 28 {
		fields["billing_day"] = "must be between 1 and 28"
	}
	if err := DecimalUint64(c.QuotaBytes).Validate(); err != nil {
		fields["quota_bytes"] = "must be a canonical unsigned decimal byte count"
	}
	if err := c.ValidateSelection(); err != nil {
		reportBudget = errors.Is(err, ErrReportBudget)
		reportBudgetFields = len(err.(*ValidationError).Fields)
		for field, message := range err.(*ValidationError).Fields {
			fields[field] = message
		}
	}
	if len(c.ProbeTargets) > 5 {
		fields["probe_targets"] = "must contain at most 5 entries"
	}
	for _, target := range c.ProbeTargets {
		if strings.TrimSpace(target) == "" || len(target) > 1024 {
			fields["probe_targets"] = "entries must be nonempty and at most 1024 bytes"
		}
	}
	if len(fields) != 0 {
		return &ValidationError{Fields: fields, reportBudget: reportBudget && len(fields) == reportBudgetFields}
	}
	return nil
}

// ValidateSelection applies the same bounded device selection contract at
// configuration persistence, server updates, and collector reconfiguration.
func (c DesiredConfig) ValidateSelection() error {
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
		fields = make(map[string]string, 2)
		if len(c.DiskPaths) != 0 {
			fields["disk_paths"] = "combined JSON-escaped selections exceed the report size budget"
		}
		if len(c.Interfaces) != 0 {
			fields["interfaces"] = "combined JSON-escaped selections exceed the report size budget"
		}
		return &ValidationError{Fields: fields, reportBudget: true}
	}
	if len(fields) != 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

func validDiskPath(path string) bool {
	return len(path) <= 1024 && !strings.ContainsRune(path, 0) && filepath.IsAbs(path)
}

func validInterfaceName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 255 && !strings.ContainsAny(name, "\x00\r\n")
}

type ReportResponse struct {
	NodeID        string         `json:"node_id,omitempty"`
	AcceptedSeq   uint64         `json:"accepted_seq"`
	ServerTime    time.Time      `json:"server_time"`
	DesiredConfig *DesiredConfig `json:"desired_config,omitempty"`
	UpdateJob     *UpdateJob     `json:"update_job,omitempty"`
	TrafficAcks   []Traffic      `json:"traffic_acks,omitzero"`
}
