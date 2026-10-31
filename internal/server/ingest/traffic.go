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
	credentialEpoch    int64
	generation         uint64
	session            string
	observedGeneration uint64
	observedSession    string
	pending            map[trafficKey]trafficCounters
	committed          map[trafficKey]trafficCounters
}

// 独立于 protocol 包做校验：即使直接调用 Accept 而不经过 HTTP，
// 这些值也会成为 SQLite 整数和键。
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

func (b *trafficBuffer) matchesRun(credential []byte, credentialEpoch int64, generation uint64, session string) bool {
	return subtle.ConstantTimeCompare(b.credential[:], credential) == 1 && b.credentialEpoch == credentialEpoch && b.generation == generation && b.session == session
}

// 节点的接受闸门串行化缓冲区变更与持久化。mu 同时保护检查与关闭快照；
// 持有 mu 期间不做数据库 I/O。
func (state *nodeState) bufferTraffic(credential []byte, credentialEpoch int64, report v1.Report, generation uint64, session string, now time.Time) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.stagedTraffic = nil
	state.traffic.buffer(credential, credentialEpoch, report, generation, session, now)
}

// 保留当前运行已接受的计数，直到新到来的运行首次持久化事务提交。
// 最多缓冲两个运行，容量相同；失败的新运行不会替换已接受的运行。
func (state *nodeState) stageTraffic(credential []byte, credentialEpoch int64, report v1.Report, generation uint64, session string, now time.Time) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.traffic.pending == nil && state.stagedTraffic == nil || state.traffic.matchesRun(credential, credentialEpoch, report.RunGeneration, report.SessionID) {
		state.stagedTraffic = nil
		state.traffic.buffer(credential, credentialEpoch, report, generation, session, now)
		return
	}
	if state.stagedTraffic == nil {
		state.stagedTraffic = &trafficBuffer{}
	}
	state.stagedTraffic.buffer(credential, credentialEpoch, report, generation, session, now)
}

// buffer 要求持有所属 nodeState 的 mu。
func (b *trafficBuffer) buffer(credential []byte, credentialEpoch int64, report v1.Report, generation uint64, session string, now time.Time) {
	if !b.matchesRun(credential, credentialEpoch, report.RunGeneration, report.SessionID) {
		*b = trafficBuffer{credentialEpoch: credentialEpoch, generation: report.RunGeneration, session: report.SessionID, observedGeneration: generation, observedSession: session, pending: make(map[trafficKey]trafficCounters), committed: make(map[trafficKey]trafficCounters)}
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

// Acks 覆盖已提交的计数以及服务端判定为过期而丢弃的条目。
func (state *nodeState) trafficAcks(entries []v1.Traffic, now time.Time) []v1.Traffic {
	state.mu.Lock()
	defer state.mu.Unlock()
	var acks []v1.Traffic
	for _, entry := range entries {
		if entry.PeriodStart.After(now.Add(time.Hour)) {
			continue
		}
		// 过期的桶有意地直接确认而不存储。
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
	// 调用方须按本事务内读取的已认证节点身份校验缓冲区，清除旧身份的数据。
	// acceptGate 保证 I/O 期间缓冲区稳定。
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

// 每次观测到持久化节点身份时调用，须持有 state.mu。
func (state *nodeState) fenceTraffic(credential []byte, credentialEpoch int64, generation uint64, session string) {
	b := &state.traffic
	if b.pending != nil && !b.matchesObservedIdentity(credential, credentialEpoch, generation, session) {
		*b = trafficBuffer{}
	}
	if state.stagedTraffic != nil && !state.stagedTraffic.matchesObservedIdentity(credential, credentialEpoch, generation, session) {
		state.stagedTraffic = nil
	}
}

func (b *trafficBuffer) matchesObservedIdentity(credential []byte, credentialEpoch int64, generation uint64, session string) bool {
	return subtle.ConstantTimeCompare(b.credential[:], credential) == 1 && b.credentialEpoch == credentialEpoch && b.observedGeneration == generation && b.observedSession == session
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

// drainTraffic 在停止接受上报后运行。每个事务内重新检查持久化身份，
// 使删除、轮换和进程替换对待处理计数与普通上报持久化采用相同的隔离规则。
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
	var credentialEpoch int64
	var generation, session, status, runtimeJSON string
	err = tx.QueryRowContext(ctx, `SELECT token_hash,credential_epoch,run_generation,report_session_id,status,agent_runtime FROM nodes WHERE id=?`, id).Scan(&credential, &credentialEpoch, &generation, &session, &status, &runtimeJSON)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows || len(credential) == 0 || status == "disabled" {
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
	state.fenceTraffic(credential, credentialEpoch, observedGeneration, session)
	state.fenceProbes(credential, credentialEpoch, observedGeneration, session)
	state.mu.Unlock()
	now := s.now().UTC()
	committed, err := state.persistTraffic(ctx, tx, id, now)
	if err != nil {
		return err
	}
	probes := state.copyProbes(credential, credentialEpoch, observedGeneration, session)
	runtime, err := decodeRuntime(runtimeJSON)
	if err != nil {
		return err
	}
	probes.retainTargets(runtime.ProbeTargets)
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
