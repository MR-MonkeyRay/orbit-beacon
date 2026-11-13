// Package services monitors server-owned HTTP, TCP and ICMP endpoints.
package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/uuid"
)

var ErrNotFound = errors.New("service not found")
var ErrCapacity = errors.New("maximum of 100 services reached")

type Service struct {
	// Internal generation fence for scheduled work across database switches.
	generation         uint64
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	GroupID            *string      `json:"group_id"`
	Type               string       `json:"type"`
	Target             string       `json:"target"`
	IntervalSeconds    int          `json:"interval_seconds"`
	FailureThreshold   int          `json:"failure_threshold"`
	TimeoutSeconds     int          `json:"timeout_seconds"`
	Enabled            bool         `json:"enabled"`
	Public             bool         `json:"public"`
	DisplayOrder       int          `json:"display_order"`
	Notify             bool         `json:"notify"`
	LatencyThresholdMS float64      `json:"latency_threshold_ms"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
	SilencedUntil      *time.Time   `json:"silenced_until"`
	Latest             *Result      `json:"latest"`
	Availability24H    *float64     `json:"availability_24h"`
	RecentHours        [24]*float64 `json:"recent_hours"`
}

type Patch struct {
	GroupID            json.RawMessage `json:"group_id"`
	Name               *string         `json:"name"`
	Type               *string         `json:"type"`
	Target             *string         `json:"target"`
	IntervalSeconds    *int            `json:"interval_seconds"`
	FailureThreshold   *int            `json:"failure_threshold"`
	TimeoutSeconds     *int            `json:"timeout_seconds"`
	Enabled            *bool           `json:"enabled"`
	Public             *bool           `json:"public"`
	DisplayOrder       *int            `json:"display_order"`
	Notify             *bool           `json:"notify"`
	LatencyThresholdMS *float64        `json:"latency_threshold_ms"`
}

func (p Patch) apply(s *Service) {
	if p.Name != nil {
		s.Name = strings.TrimSpace(*p.Name)
	}
	if p.Type != nil {
		s.Type = *p.Type
	}
	if p.Target != nil {
		s.Target = strings.TrimSpace(*p.Target)
	}
	if p.IntervalSeconds != nil {
		s.IntervalSeconds = *p.IntervalSeconds
	}
	if p.FailureThreshold != nil {
		s.FailureThreshold = *p.FailureThreshold
	}
	if p.TimeoutSeconds != nil {
		s.TimeoutSeconds = *p.TimeoutSeconds
	}
	if p.Enabled != nil {
		s.Enabled = *p.Enabled
	}
	if p.Public != nil {
		s.Public = *p.Public
	}
	if p.DisplayOrder != nil {
		s.DisplayOrder = *p.DisplayOrder
	}
	if p.Notify != nil {
		s.Notify = *p.Notify
	}
	if p.LatencyThresholdMS != nil {
		s.LatencyThresholdMS = *p.LatencyThresholdMS
	}
}
func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 253 || strings.ContainsAny(host, " :/%[]@?#\\") {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func Validate(s Service) error {
	fields := map[string]string{}
	if s.GroupID != nil && !uuid.ValidUUID(*s.GroupID) {
		fields["group_id"] = "must be a group UUID or null"
	}
	if utf8.RuneCountInString(s.Name) < 1 || utf8.RuneCountInString(s.Name) > 100 || !utf8.ValidString(s.Name) || strings.IndexFunc(s.Name, unicode.IsControl) >= 0 {
		fields["name"] = "must contain 1 to 100 characters without controls"
	}
	if s.FailureThreshold < 1 || s.FailureThreshold > 10 {
		fields["failure_threshold"] = "must be between 1 and 10"
	}
	if s.IntervalSeconds < 30 || s.IntervalSeconds > 3600 {
		fields["interval_seconds"] = "must be between 30 and 3600"
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 30 || s.TimeoutSeconds >= s.IntervalSeconds {
		fields["timeout_seconds"] = "must be between 1 and 30 and less than interval"
	}
	if math.IsNaN(s.LatencyThresholdMS) || math.IsInf(s.LatencyThresholdMS, 0) || s.LatencyThresholdMS < 0 || s.LatencyThresholdMS > 3600000 {
		fields["latency_threshold_ms"] = "must be between 0 and 3600000"
	}
	if s.DisplayOrder < -1000000 || s.DisplayOrder > 1000000 {
		fields["display_order"] = "must be between -1000000 and 1000000"
	}
	valid := len(s.Target) > 0 && len(s.Target) <= 2048 && strings.IndexFunc(s.Target, unicode.IsControl) < 0
	switch s.Type {
	case "http":
		u, e := url.Parse(s.Target)
		valid = valid && e == nil
		if valid {
			valid = u.IsAbs() && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && validHost(u.Hostname()) && u.Fragment == ""
			if u.Port() != "" {
				p, e := strconv.Atoi(u.Port())
				valid = valid && e == nil && p >= 1 && p <= 65535
			}
		}
	case "tcp":
		host, port, e := net.SplitHostPort(s.Target)
		p, pe := strconv.Atoi(port)
		valid = valid && e == nil && pe == nil && p >= 1 && p <= 65535 && validHost(host)
	case "icmp":
		valid = valid && validHost(s.Target)
	default:
		fields["type"] = "must be one of http, tcp, icmp"
	}
	if !valid {
		fields["target"] = "must match the service type without credentials"
	}
	if len(fields) > 0 {
		return &validation.Error{Fields: fields}
	}
	return nil
}

type Result struct {
	OK           bool       `json:"ok"`
	Status       string     `json:"status"`
	LatencyMS    *float64   `json:"latency_ms"`
	Error        string     `json:"error"`
	StatusCode   *int       `json:"status_code"`
	PacketLoss   *float64   `json:"packet_loss"`
	TLSExpiresAt *time.Time `json:"tls_expires_at"`
	CheckedAt    time.Time  `json:"checked_at"`
	LatencyHigh  bool       `json:"latency_high"`
	// Persistent hysteresis and certificate pacing survive server restarts.
	Failures               int    `json:"consecutive_failures"`
	SlowChecks             int    `json:"consecutive_slow"`
	CertificateNotifiedDay string `json:"certificate_notified_day"`
	DownSilenced           bool   `json:"down_silenced"`
	LatencySilenced        bool   `json:"latency_silenced"`
}

type Event struct {
	ID           string     `json:"id"`
	ServiceID    string     `json:"service_id"`
	Name         string     `json:"name"`
	Target       string     `json:"target"`
	Error        string     `json:"error"`
	Transition   string     `json:"transition"`
	LatencyMS    *float64   `json:"latency_ms"`
	TLSExpiresAt *time.Time `json:"tls_expires_at"`
	OccurredAt   time.Time  `json:"occurred_at"`
}

// Advance counts observations, not elapsed downtime; unknown periods remain unknown.
func Advance(s Service, previous *Result, result Result) (Result, []Event) {
	return advanceSilenced(s, previous, result, false)
}

// Suppression belongs to an incident, not to its recovery timestamp. Resolve
// before releasing, so a recovery observed after a window never replays a fire.
func advanceSilenced(s Service, previous *Result, result Result, silenced bool) (Result, []Event) {
	result.Status = "unknown"
	result.DownSilenced = false
	result.LatencySilenced = false
	if previous != nil {
		result.Status = previous.Status
		result.Failures = previous.Failures
		result.SlowChecks = previous.SlowChecks
		result.LatencyHigh = previous.LatencyHigh
		result.CertificateNotifiedDay = previous.CertificateNotifiedDay
		result.DownSilenced = previous.DownSilenced
		result.LatencySilenced = previous.LatencySilenced
	}
	var events []Event
	emit := func(transition string) {
		if s.Notify {
			events = append(events, Event{ServiceID: s.ID, Name: s.Name, Target: s.Target, Error: result.Error, Transition: transition, LatencyMS: result.LatencyMS, TLSExpiresAt: result.TLSExpiresAt, OccurredAt: result.CheckedAt})
		}
	}
	if result.OK {
		result.Failures = 0
		if result.Status == "down" {
			if !result.DownSilenced {
				emit("recovered")
			}
			result.DownSilenced = false
		}
		result.Status = "up"
		if s.LatencyThresholdMS > 0 && result.LatencyMS != nil && *result.LatencyMS > s.LatencyThresholdMS {
			result.SlowChecks = min(3, result.SlowChecks+1)
			if result.SlowChecks == 3 && !result.LatencyHigh {
				result.LatencyHigh = true
				result.LatencySilenced = s.Notify && silenced
				if !silenced {
					emit("latency_high")
				}
			}
		} else {
			result.SlowChecks = 0
			if result.LatencyHigh {
				result.LatencyHigh = false
				if !result.LatencySilenced {
					emit("latency_recovered")
				}
				result.LatencySilenced = false
			}
		}
	} else {
		result.Failures = min(s.FailureThreshold, result.Failures+1)
		result.SlowChecks = 0
		if result.Failures >= s.FailureThreshold && result.Status != "down" {
			result.Status = "down"
			result.DownSilenced = s.Notify && silenced
			if !silenced {
				emit("down")
			}
		}
	}
	if s.Notify && !silenced {
		if result.DownSilenced {
			emit("down")
			result.DownSilenced = false
		}
		if result.LatencySilenced {
			emit("latency_high")
			result.LatencySilenced = false
		}
	}
	if s.Notify && !silenced && result.TLSExpiresAt != nil && result.TLSExpiresAt.Sub(result.CheckedAt) <= 14*24*time.Hour && result.CertificateNotifiedDay != result.CheckedAt.UTC().Format(time.DateOnly) {
		emit("certificate_expiring")
		result.CertificateNotifiedDay = result.CheckedAt.UTC().Format(time.DateOnly)
	}
	return result, events
}

func ValidateResult(r Result) error {
	if r.CheckedAt.IsZero() || r.Status != "up" && r.Status != "down" && r.Status != "unknown" || r.Failures < 0 || r.Failures > 10 || r.SlowChecks < 0 || r.SlowChecks > 3 {
		return errors.New("invalid service result")
	}
	if r.DownSilenced && r.Status != "down" || r.LatencySilenced && !r.LatencyHigh {
		return errors.New("invalid silenced service incident")
	}
	for _, v := range []*float64{r.LatencyMS, r.PacketLoss} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0) {
			return errors.New("invalid service measurement")
		}
	}
	if r.PacketLoss != nil && *r.PacketLoss > 100 {
		return errors.New("invalid packet loss")
	}
	if r.StatusCode != nil && (*r.StatusCode < 100 || *r.StatusCode > 599) {
		return errors.New("invalid status code")
	}
	switch r.Error {
	case "", "permission_denied", "unsupported", "timeout", "refused", "dns", "http_status", "denied", "error":
	default:
		return errors.New("invalid service error")
	}
	if r.CertificateNotifiedDay != "" {
		if _, e := time.Parse(time.DateOnly, r.CertificateNotifiedDay); e != nil {
			return e
		}
	}
	return nil
}

// Only group_id is nullable; RawMessage distinguishes omission from explicit null.
func (p *Patch) UnmarshalJSON(raw []byte) error {
	type plain Patch
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil {
		return e
	}
	for name, value := range fields {
		if name == "failure_threshold" {
			var n int
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &n) != nil || n < 1 || n > 10 {
				return validation.Invalid(name, "must be an integer between 1 and 10")
			}
		}
		if name != "group_id" && string(value) == "null" {
			return validation.Invalid(name, "must not be null")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*plain)(p))
}
