package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/timekey"
)

const trafficBufferLimit = 48
const trafficHourlyRetention = 35 * 24 * time.Hour

type trafficKey struct{ epoch, period string }
type trafficCounters struct{ rx, tx int64 }
type trafficBuffer struct {
	credential         [sha256.Size]byte
	generation         uint64
	session            string
	observedGeneration uint64
	observedSession    string
	pending            map[trafficKey]trafficCounters
	committed          map[trafficKey]trafficCounters
}

// Validate independently of the protocol package: these values become SQLite
// integers and keys even when Accept is called directly rather than over HTTP.
func validateTraffic(entries []v1.Traffic) error {
	if len(entries) > 24 {
		return fmt.Errorf("traffic must contain at most 24 entries")
	}
	seen := make(map[trafficKey]bool, len(entries))
	for _, entry := range entries {
		if len(entry.Epoch) < 1 || len(entry.Epoch) > 64 {
			return fmt.Errorf("invalid traffic epoch")
		}
		for _, c := range entry.Epoch {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return fmt.Errorf("invalid traffic epoch")
			}
		}
		_, offset := entry.PeriodStart.Zone()
		if entry.PeriodStart.IsZero() || offset != 0 || !entry.PeriodStart.Equal(entry.PeriodStart.Truncate(time.Hour)) {
			return fmt.Errorf("traffic period must be a UTC hour")
		}
		if err := entry.RxBytes.Validate(); err != nil {
			return err
		}
		if err := entry.TxBytes.Validate(); err != nil {
			return err
		}
		if _, err := strconv.ParseInt(string(entry.RxBytes), 10, 64); err != nil {
			return fmt.Errorf("traffic rx_bytes exceeds SQLite integer range")
		}
		if _, err := strconv.ParseInt(string(entry.TxBytes), 10, 64); err != nil {
			return fmt.Errorf("traffic tx_bytes exceeds SQLite integer range")
		}
		key := trafficKey{entry.Epoch, timekey.Format(entry.PeriodStart)}
		if seen[key] {
			return fmt.Errorf("duplicate traffic epoch and period")
		}
		seen[key] = true
	}
	return nil
}

func (b *trafficBuffer) matchesRun(credential []byte, generation uint64, session string) bool {
	return subtle.ConstantTimeCompare(b.credential[:], credential) == 1 && b.generation == generation && b.session == session
}

// The node accept gate serializes buffer changes with persistence. mu also
// protects inspection and shutdown snapshots; no database I/O holds mu.
func (state *nodeState) bufferTraffic(credential []byte, report v1.Report, generation uint64, session string, now time.Time) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.stagedTraffic = nil
	state.traffic.buffer(credential, report, generation, session, now)
}

// Keep accepted counters from the current run until the incoming run's first
// durable transaction commits. At most two runs are buffered, each with the
// same fixed capacity; a failed incoming run never replaces the accepted run.
func (state *nodeState) stageTraffic(credential []byte, report v1.Report, generation uint64, session string, now time.Time) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.traffic.pending == nil && state.stagedTraffic == nil || state.traffic.matchesRun(credential, report.RunGeneration, report.SessionID) {
		state.stagedTraffic = nil
		state.traffic.buffer(credential, report, generation, session, now)
		return
	}
	if state.stagedTraffic == nil {
		state.stagedTraffic = &trafficBuffer{}
	}
	state.stagedTraffic.buffer(credential, report, generation, session, now)
}

// buffer requires the owning nodeState.mu to be held.
func (b *trafficBuffer) buffer(credential []byte, report v1.Report, generation uint64, session string, now time.Time) {
	if !b.matchesRun(credential, report.RunGeneration, report.SessionID) {
		*b = trafficBuffer{generation: report.RunGeneration, session: report.SessionID, observedGeneration: generation, observedSession: session, pending: make(map[trafficKey]trafficCounters), committed: make(map[trafficKey]trafficCounters)}
		copy(b.credential[:], credential)
	}
	for key := range b.pending {
		period, _ := timekey.Parse(key.period)
		if period.Before(now.Add(-trafficHourlyRetention)) {
			delete(b.pending, key)
		}
	}
	for key := range b.committed {
		period, _ := timekey.Parse(key.period)
		if period.Before(now.Add(-trafficHourlyRetention)) {
			delete(b.committed, key)
		}
	}
	for _, entry := range report.Traffic {
		if entry.PeriodStart.After(now.Add(time.Hour)) || entry.PeriodStart.Before(now.Add(-trafficHourlyRetention)) {
			continue
		}
		key := trafficKey{entry.Epoch, timekey.Format(entry.PeriodStart)}
		rx, _ := strconv.ParseInt(string(entry.RxBytes), 10, 64)
		tx, _ := strconv.ParseInt(string(entry.TxBytes), 10, 64)
		if known, ok := b.committed[key]; ok && known.rx >= rx && known.tx >= tx {
			continue
		}
		prior, exists := b.pending[key]
		if !exists && len(b.pending) >= trafficBufferLimit {
			continue
		}
		b.pending[key] = trafficCounters{max(prior.rx, rx), max(prior.tx, tx)}
	}
}

// Acks cover committed counters and entries the server discards as expired.
func (state *nodeState) trafficAcks(entries []v1.Traffic, now time.Time) []v1.Traffic {
	state.mu.Lock()
	defer state.mu.Unlock()
	var acks []v1.Traffic
	for _, entry := range entries {
		if entry.PeriodStart.After(now.Add(time.Hour)) {
			continue
		}
		// Expired buckets are deliberately acknowledged without storing them.
		if entry.PeriodStart.Before(now.Add(-trafficHourlyRetention)) {
			acks = append(acks, entry)
			continue
		}
		if known, ok := state.traffic.committed[trafficKey{entry.Epoch, timekey.Format(entry.PeriodStart)}]; ok {
			entry.RxBytes = v1.DecimalUint64(strconv.FormatInt(known.rx, 10))
			entry.TxBytes = v1.DecimalUint64(strconv.FormatInt(known.tx, 10))
			acks = append(acks, entry)
		}
	}
	return acks
}

func (state *nodeState) persistTraffic(ctx context.Context, tx *sql.Tx, nodeID string, now time.Time) (map[trafficKey]trafficCounters, error) {
	// The caller must fence buffers against the authenticated node identity read
	// inside this transaction. The accept gate keeps them stable during I/O.
	state.mu.Lock()
	pending := copyTrafficPending(state.traffic.pending)
	var incoming map[trafficKey]trafficCounters
	if state.stagedTraffic != nil {
		incoming = copyTrafficPending(state.stagedTraffic.pending)
	}
	state.mu.Unlock()
	committed, err := persistTrafficPending(ctx, tx, nodeID, pending, now)
	if err != nil {
		return nil, err
	}
	if incoming != nil {
		return persistTrafficPending(ctx, tx, nodeID, incoming, now)
	}
	return committed, nil
}

func copyTrafficPending(pending map[trafficKey]trafficCounters) map[trafficKey]trafficCounters {
	owned := make(map[trafficKey]trafficCounters, len(pending))
	for key, value := range pending {
		owned[key] = value
	}
	return owned
}

func persistTrafficPending(ctx context.Context, tx *sql.Tx, nodeID string, pending map[trafficKey]trafficCounters, now time.Time) (map[trafficKey]trafficCounters, error) {
	committed := make(map[trafficKey]trafficCounters, len(pending))
	for key, value := range pending {
		period, _ := timekey.Parse(key.period)
		if period.Before(now.Add(-trafficHourlyRetention)) {
			continue
		}
		var stored trafficCounters
		err := tx.QueryRowContext(ctx, `INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(node_id,epoch,period_start) DO UPDATE SET rx_bytes=MAX(traffic_hourly.rx_bytes,excluded.rx_bytes),tx_bytes=MAX(traffic_hourly.tx_bytes,excluded.tx_bytes),updated_at=MAX(traffic_hourly.updated_at,excluded.updated_at) RETURNING rx_bytes,tx_bytes`, nodeID, key.epoch, key.period, value.rx, value.tx, timekey.Format(now)).Scan(&stored.rx, &stored.tx)
		if err != nil {
			return nil, err
		}
		committed[key] = stored
	}
	return committed, nil
}

// Called with state.mu held whenever a durable node identity is observed.
func (state *nodeState) fenceTraffic(credential []byte, generation uint64, session string) {
	b := &state.traffic
	if b.pending != nil && !b.matchesObservedIdentity(credential, generation, session) {
		*b = trafficBuffer{}
	}
	if state.stagedTraffic != nil && !state.stagedTraffic.matchesObservedIdentity(credential, generation, session) {
		state.stagedTraffic = nil
	}
}

func (b *trafficBuffer) matchesObservedIdentity(credential []byte, generation uint64, session string) bool {
	return subtle.ConstantTimeCompare(b.credential[:], credential) == 1 && b.observedGeneration == generation && b.observedSession == session
}

func (state *nodeState) commitTraffic(committed map[trafficKey]trafficCounters, now time.Time) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stagedTraffic != nil {
		state.traffic = *state.stagedTraffic
		state.stagedTraffic = nil
	}
	b := &state.traffic
	b.observedGeneration, b.observedSession = b.generation, b.session
	for key, value := range committed {
		if _, ok := b.committed[key]; !ok && len(b.committed) >= trafficBufferLimit {
			for old := range b.committed {
				delete(b.committed, old)
				break
			}
		}
		b.committed[key] = value
		delete(b.pending, key)
	}
	for key := range b.pending {
		period, _ := timekey.Parse(key.period)
		if period.Before(now.Add(-trafficHourlyRetention)) {
			delete(b.pending, key)
		}
	}
}

// Drain runs after report acceptance has stopped. Recheck the durable identity
// inside each transaction so deletion, rotation and process replacement fence
// pending counters exactly as they fence ordinary report persistence.
func (s *Service) drainTraffic(ctx context.Context) error {
	s.mu.Lock()
	states := make(map[string]*nodeState, len(s.states))
	for id, state := range s.states {
		states[id] = state
	}
	s.mu.Unlock()
	var firstErr error
	for id, state := range states {
		if err := s.drainNodeTraffic(ctx, id, state); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("drain traffic for node %q: %w", id, err)
		}
	}
	return firstErr
}

func (s *Service) drainNodeTraffic(ctx context.Context, id string, state *nodeState) error {
	state.mu.Lock()
	pending := len(state.traffic.pending) > 0 || (state.stagedTraffic != nil && len(state.stagedTraffic.pending) > 0) || state.probesPending()
	state.mu.Unlock()
	if !pending {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var credential []byte
	var generation, session, status string
	err = tx.QueryRowContext(ctx, `SELECT token_hash,run_generation,report_session_id,status FROM nodes WHERE id=?`, id).Scan(&credential, &generation, &session, &status)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows || status == "disabled" {
		state.mu.Lock()
		state.traffic = trafficBuffer{}
		state.stagedTraffic = nil
		state.probes = probeBuffer{}
		state.mu.Unlock()
		return nil
	}
	observedGeneration, err := strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return err
	}
	state.mu.Lock()
	state.fenceTraffic(credential, observedGeneration, session)
	state.fenceProbes(credential, observedGeneration, session)
	state.mu.Unlock()
	now := s.now().UTC()
	committed, err := state.persistTraffic(ctx, tx, id, now)
	if err != nil {
		return err
	}
	probes := state.copyProbes(credential, observedGeneration, session)
	if err := probes.persist(ctx, tx, id, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	state.commitTraffic(committed, now)
	state.commitProbes(probes, observedGeneration, session)
	return nil
}
