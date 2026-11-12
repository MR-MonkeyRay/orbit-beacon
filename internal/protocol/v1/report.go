package v1

import (
	"errors"
	"strings"
	"time"
)

const MaxProbeTargets = 5

type Report struct {
	CommandResults  []CommandResult `json:"command_results,omitzero"`
	ProtocolVersion int             `json:"protocol_version"`
	RunGeneration   uint64          `json:"run_generation"`
	SessionID       string          `json:"session_id"`
	Seq             uint64          `json:"seq"`
	SampledAt       time.Time       `json:"sampled_at"`
	System          *System         `json:"system,omitempty"`
	Metrics         Metrics         `json:"metrics"`
	Traffic         []Traffic       `json:"traffic"`
	ProbeResults    []ProbeResult   `json:"probe_results,omitzero"`
}

func (r Report) Validate() error {
	if r.ProtocolVersion != ProtocolVersion {
		return ErrProtocolUnsupported
	}
	if r.RunGeneration == 0 || r.Seq == 0 || strings.TrimSpace(r.SessionID) == "" || len(r.SessionID) > 255 || r.SampledAt.IsZero() {
		return errors.New("invalid report identity or time")
	}
	if r.System != nil {
		if err := r.System.Validate(); err != nil {
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
	if len(r.CommandResults) > 2 {
		return errors.New("too many command results")
	}
	for _, result := range r.CommandResults {
		if err := result.Validate(); err != nil {
			return err
		}
	}
	if len(r.ProbeResults) > MaxProbeTargets {
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

type ReportResponse struct {
	Command     *Command  `json:"command,omitempty"`
	NodeID      string    `json:"node_id,omitempty"`
	AcceptedSeq uint64    `json:"accepted_seq"`
	ServerTime  time.Time `json:"server_time"`
	TrafficAcks []Traffic `json:"traffic_acks,omitzero"`
}
