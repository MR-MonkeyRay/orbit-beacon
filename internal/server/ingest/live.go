package ingest

import (
	"encoding/json"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

// ProjectLiveRecord reads only the receipt clock under the existing state lock.
// It neither allocates report copies nor decodes a report for fleet presence.
func (s *Service) ProjectLiveRecord(record *nodes.LiveRecord, now time.Time) nodes.LiveNode {
	out := record.LiveNode
	identity := &record.Identity
	if identity.HasCredential && identity.Generation != 0 && identity.Session != "" {
		state := s.acquireExistingState(record.ID)
		if state != nil {
			state.mu.Lock()
			if state.matchesIdentity(identity.Credential[:], identity.CredentialEpoch, identity.Generation, identity.Session) && (out.LastSeen == nil || state.receivedAt.After(*out.LastSeen)) {
				seen := state.receivedAt
				out.LastSeen = &seen
			}
			state.mu.Unlock()
			s.releaseState(state)
		}
	}
	out.Presence = nodes.Presence(out.Status, identity.HasCredential, out.LastSeen, now)
	return out
}

// ProjectLiveDetail uses the identity and observations read in one durable row.
// Runtime follows the same accepted sample even between a durable transaction's
// commit and memory publication. After restart durable runtime can stand alone.
func (s *Service) ProjectLiveDetail(record *nodes.LiveDetailRecord, now time.Time) (*LatestReport, error) {
	identity := &record.Identity
	if !identity.HasCredential || identity.Generation == 0 || identity.Session == "" {
		record.Node.Presence = nodes.Presence(record.Node.Status, identity.HasCredential, record.Node.LastSeen, now)
		return nil, nil
	}
	state := s.acquireExistingState(record.Node.ID)
	if state == nil {
		return nil, nil
	}
	defer s.releaseState(state)
	state.mu.Lock()
	if !state.matchesIdentity(identity.Credential[:], identity.CredentialEpoch, identity.Generation, identity.Session) {
		state.mu.Unlock()
		return nil, nil
	}
	// Published report buffers are immutable, so holding the reference is
	// sufficient while decoding this one detail, even if a new report arrives.
	body := state.latest
	received := state.receivedAt
	state.mu.Unlock()
	if record.Node.LastSeen == nil || received.After(*record.Node.LastSeen) {
		record.Node.LastSeen = &received
	}
	record.Node.Presence = nodes.Presence(record.Node.Status, identity.HasCredential, record.Node.LastSeen, now)
	var report v1.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, err
	}
	record.Runtime = &report.Runtime
	return &LatestReport{Report: report, ReceivedAt: received}, nil
}
