package v1

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const ProtocolVersion = 1

const MaxTrafficEntries = 24

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
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version,omitempty"`
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
	if len(s.AgentVersion) > 64 {
		return errors.New("agent version exceeds field limit")
	}
	for i := range len(s.AgentVersion) {
		character := s.AgentVersion[i]
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '.' && character != '_' && character != '+' && character != '-' {
			return errors.New("agent version contains invalid characters")
		}
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
	if len(t.Epoch) < 1 || len(t.Epoch) > 64 {
		return errors.New("invalid traffic identity")
	}
	for i := range len(t.Epoch) {
		if (t.Epoch[i] < '0' || t.Epoch[i] > '9') && (t.Epoch[i] < 'a' || t.Epoch[i] > 'f') {
			return errors.New("traffic epoch must contain lowercase hexadecimal characters")
		}
	}
	if t.PeriodStart.IsZero() || t.PeriodStart.Location() != time.UTC ||
		t.PeriodStart.Minute() != 0 || t.PeriodStart.Second() != 0 || t.PeriodStart.Nanosecond() != 0 {
		return errors.New("traffic period start must be a UTC hour")
	}
	if err := t.RxBytes.Validate(); err != nil {
		return err
	}
	if err := t.TxBytes.Validate(); err != nil {
		return err
	}
	for _, value := range []DecimalUint64{t.RxBytes, t.TxBytes} {
		parsed, _ := strconv.ParseUint(string(value), 10, 64)
		if parsed > math.MaxInt64 {
			return errors.New("traffic counter exceeds int64 range")
		}
	}
	return nil
}

type ProbeResult struct {
	Target     string    `json:"target"`
	OK         bool      `json:"ok"`
	LatencyMS  *float64  `json:"latency_ms"`
	Error      string    `json:"error"`
	StatusCode *int      `json:"status_code"`
	CheckedAt  time.Time `json:"checked_at"`
}

func (p ProbeResult) Validate() error {
	if err := ValidateProbeTarget(p.Target); err != nil {
		return err
	}
	if p.OK != (p.Error == "") {
		return errors.New("probe result status and error disagree")
	}
	if p.OK && p.LatencyMS == nil {
		return errors.New("successful probe result requires latency")
	}
	if !p.OK && p.LatencyMS != nil {
		return errors.New("failed probe result must not include latency")
	}
	if p.LatencyMS != nil && (math.IsNaN(*p.LatencyMS) || math.IsInf(*p.LatencyMS, 0) || *p.LatencyMS < 0 || *p.LatencyMS > 60000) {
		return errors.New("probe result latency must be finite and between 0 and 60000 milliseconds")
	}
	validError := p.Error == "" || p.Error == "timeout" || p.Error == "refused" || p.Error == "dns" ||
		p.Error == "tls" || p.Error == "http_status" || p.Error == "unreachable" || p.Error == "unsupported" || p.Error == "error"
	if !validError {
		return errors.New("invalid probe result error")
	}
	if p.Error == "http_status" && p.StatusCode == nil {
		return errors.New("HTTP status probe result requires status code")
	}
	if p.StatusCode != nil {
		if !strings.HasPrefix(p.Target, "http://") && !strings.HasPrefix(p.Target, "https://") {
			return errors.New("non-HTTP probe result must not include status code")
		}
		if *p.StatusCode < 100 || *p.StatusCode > 599 {
			return errors.New("probe result status code must be between 100 and 599")
		}
	}
	if p.CheckedAt.IsZero() {
		return errors.New("probe result checked time is required")
	}
	return nil
}

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

// Validate checks the persisted/report configuration error without side effects.
func (e ConfigError) Validate() error {
	if e.Revision < 0 || e.Code == "" || len(e.Code) > 64 {
		return errors.New("invalid configuration error")
	}
	return nil
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
	if r.System != nil {
		if err := r.System.Validate(); err != nil {
			return err
		}
	}
	if r.ConfigError != nil {
		if err := r.ConfigError.Validate(); err != nil {
			return err
		}
	}
	if err := r.Metrics.Validate(); err != nil {
		return err
	}
	if len(r.Traffic) > MaxTrafficEntries {
		return errors.New("too many traffic entries")
	}
	trafficIdentities := make(map[struct {
		epoch           string
		periodStartUnix int64
	}]struct{}, len(r.Traffic))
	for _, traffic := range r.Traffic {
		if err := traffic.Validate(); err != nil {
			return err
		}
		identity := struct {
			epoch           string
			periodStartUnix int64
		}{traffic.Epoch, traffic.PeriodStart.Unix()}
		if _, exists := trafficIdentities[identity]; exists {
			return errors.New("duplicate traffic entry")
		}
		trafficIdentities[identity] = struct{}{}
	}
	if len(r.ProbeResults) > 5 {
		return errors.New("too many probe results")
	}
	probeTargets := make(map[string]struct{}, len(r.ProbeResults))
	for _, result := range r.ProbeResults {
		if err := result.Validate(); err != nil {
			return err
		}
		if _, exists := probeTargets[result.Target]; exists {
			return errors.New("duplicate probe result target")
		}
		probeTargets[result.Target] = struct{}{}
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
	} else {
		seen := make(map[string]struct{}, len(c.ProbeTargets))
		for _, target := range c.ProbeTargets {
			if err := ValidateProbeTarget(target); err != nil {
				fields["probe_targets"] = err.Error()
				break
			}
			if _, exists := seen[target]; exists {
				fields["probe_targets"] = "entries must be unique"
				break
			}
			seen[target] = struct{}{}
		}
	}
	if len(fields) != 0 {
		return &ValidationError{Fields: fields, reportBudget: reportBudget && len(fields) == reportBudgetFields}
	}
	return nil
}

// ValidateProbeTarget validates one service probe target in its wire format.
func ValidateProbeTarget(target string) error {
	if target == "" {
		return errors.New("entries must be nonempty")
	}
	if len(target) > 1024 {
		return errors.New("entries must be at most 1024 bytes")
	}
	for _, character := range target {
		if unicode.IsSpace(character) || character == '\ufeff' {
			return errors.New("entries must not contain whitespace")
		}
	}

	if address, ok := strings.CutPrefix(target, "tcp://"); ok {
		host, portText, err := net.SplitHostPort(address)
		if err != nil || host == "" || !validProbeHost(addressHost(address, host)) {
			return errors.New("TCP targets must use tcp://host:port with a valid host")
		}
		if portText == "" || !onlyDigits(portText) {
			return errors.New("TCP target port must be between 1 and 65535")
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return errors.New("TCP target port must be between 1 and 65535")
		}
		return nil
	}

	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if strings.ContainsAny(target, "#\\") {
			return errors.New("HTTP targets must not contain fragments or backslashes")
		}
		parsed, err := url.Parse(target)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
			return errors.New("HTTP targets must be absolute URLs with a host")
		}
		if parsed.User != nil {
			return errors.New("HTTP targets must not contain user information")
		}
		if strings.HasPrefix(parsed.Host, "[") && net.ParseIP(parsed.Hostname()) == nil {
			return errors.New("HTTP targets must use a valid bracketed IPv6 address")
		}
		// Validate the host as written: URL parsers decode or rewrite hosts
		// (percent escapes, IPv4 shorthand, IDNA) differently across Go and browsers.
		if !validProbeHost(rawURLHost(target)) {
			return errors.New("HTTP targets need a valid DNS name, IPv4 address, or bracketed IPv6 address")
		}
		if port := parsed.Port(); port != "" {
			value, err := strconv.Atoi(port)
			if err != nil || value > 65535 {
				return errors.New("HTTP target port must be between 0 and 65535")
			}
		}
		return nil
	}

	if host, ok := strings.CutPrefix(target, "icmp://"); ok {
		if host == "" || !validProbeHost(host) {
			return errors.New("ICMP targets must use icmp://host with a valid host")
		}
		return nil
	}
	return errors.New("targets must use tcp://, http://, https://, or icmp://")
}

// rawURLHost returns the authority's host exactly as written, without port.
func rawURLHost(target string) string {
	rest := target[strings.Index(target, "//")+2:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if strings.HasPrefix(rest, "[") {
		if i := strings.Index(rest, "]"); i >= 0 {
			return rest[:i+1]
		}
		return rest
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func addressHost(address, splitHost string) string {
	if strings.HasPrefix(address, "[") {
		return "[" + splitHost + "]"
	}
	return splitHost
}

func validProbeHost(host string) bool {
	if strings.HasPrefix(host, "[") {
		if !strings.HasSuffix(host, "]") || !strings.Contains(host, ":") {
			return false
		}
		return net.ParseIP(host[1:len(host)-1]) != nil
	}
	if strings.ContainsAny(host, ":[]") {
		return false
	}
	if onlyDigitsAndDots(host) {
		return validProbeIPv4(host)
	}
	if host == "" || len(host) > 253 {
		return false
	}
	name := strings.TrimSuffix(host, ".")
	if name == "" {
		return false
	}
	labels := strings.Split(name, ".")
	// A numeric final label makes browsers parse the host as IPv4 (e.g. 0x7f.1).
	if last := strings.ToLower(labels[len(labels)-1]); onlyDigitsAndDots(last) || strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "" {
		return false
	}
	for _, label := range labels {
		if len(label) < 1 || len(label) > 63 || !asciiAlphaNumeric(label[0]) || !asciiAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			if !asciiAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func onlyDigitsAndDots(value string) bool {
	for i := range len(value) {
		if (value[i] < '0' || value[i] > '9') && value[i] != '.' {
			return false
		}
	}
	return true
}

func onlyDigits(value string) bool {
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func validProbeIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		value, err := strconv.Atoi(part)
		if err != nil || value > 255 {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
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
		if len(c.ProbeTargets) != 0 {
			fields["probe_targets"] = "combined JSON-escaped selections exceed the report size budget"
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
