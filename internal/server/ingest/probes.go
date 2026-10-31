package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"slices"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/timekey"
)

const probeTargetLimit = 5

// 在每秒一次上报的准入速率下，该限制超过正常的 30 秒持久化间隔（含突发）。
// 溢出时通过背压保留已接受的检查。
const probePendingLimit = 64
const probeRetention = 7 * 24 * time.Hour

type probeTargetBuffer struct {
	counted time.Time
	pending []v1.ProbeResult
}

type probeBuffer struct {
	credential      [sha256.Size]byte
	credentialEpoch int64
	generation      uint64
	session         string
	targets         map[string]probeTargetBuffer
}

type probeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// 节点的接受闸门串行化缓冲区变更与提交。身份是缓冲时观测到的持久化身份，
// 包括跨进程推进的情况。
func (state *nodeState) fenceProbes(credential []byte, credentialEpoch int64, generation uint64, session string) {
	b := &state.probes
	if b.targets != nil && (subtle.ConstantTimeCompare(b.credential[:], credential) != 1 || b.credentialEpoch != credentialEpoch || b.generation != generation || b.session != session) {
		*b = probeBuffer{}
	}
}

func (state *nodeState) copyProbes(credential []byte, credentialEpoch int64, generation uint64, session string) probeBuffer {
	state.mu.Lock()
	defer state.mu.Unlock()
	b := probeBuffer{credentialEpoch: credentialEpoch, generation: generation, session: session, targets: make(map[string]probeTargetBuffer, probeTargetLimit)}
	copy(b.credential[:], credential)
	for target, entry := range state.probes.targets {
		entry.pending = append([]v1.ProbeResult(nil), entry.pending...)
		b.targets[target] = entry
	}
	return b
}

func (state *nodeState) commitProbes(b probeBuffer, generation uint64, session string) {
	b.generation, b.session = generation, session
	state.mu.Lock()
	state.probes = b
	state.mu.Unlock()
}

func probeWatermark(ctx context.Context, q probeQueryer, nodeID, target string) (time.Time, error) {
	var text string
	err := q.QueryRowContext(ctx, `SELECT checked_at FROM probe_latest WHERE node_id=? AND target=?`, nodeID, target).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return timekey.Parse(text)
}

func (b *probeBuffer) retainTargets(targets []string) {
	for target := range b.targets {
		if !slices.Contains(targets, target) {
			delete(b.targets, target)
		}
	}
}

func (b *probeBuffer) buffer(ctx context.Context, q probeQueryer, nodeID string, results []v1.ProbeResult, targets []string) error {
	for _, result := range results {
		if !slices.Contains(targets, result.Target) {
			continue
		}
		entry, exists := b.targets[result.Target]
		if !exists {
			if len(b.targets) >= probeTargetLimit {
				oldest := ""
				for target, candidate := range b.targets {
					if slices.Contains(targets, target) || len(candidate.pending) != 0 {
						continue
					}
					if oldest == "" || candidate.counted.Before(b.targets[oldest].counted) {
						oldest = target
					}
				}
				if oldest == "" {
					return ErrWriterBusy
				}
				delete(b.targets, oldest)
			}
			counted, err := probeWatermark(ctx, q, nodeID, result.Target)
			if err != nil {
				return err
			}
			entry.counted = counted
		}
		if !result.CheckedAt.After(entry.counted) {
			b.targets[result.Target] = entry
			continue
		}
		if len(entry.pending) >= probePendingLimit {
			return ErrWriterBusy
		}
		// 报告属于其调用方；指针值也要取得所有权。
		if result.LatencyMS != nil {
			value := *result.LatencyMS
			result.LatencyMS = &value
		}
		if result.StatusCode != nil {
			value := *result.StatusCode
			result.StatusCode = &value
		}
		entry.pending = append(entry.pending, result)
		entry.counted = result.CheckedAt
		b.targets[result.Target] = entry
	}
	return nil
}

func (b *probeBuffer) persist(ctx context.Context, tx *sql.Tx, nodeID string, now time.Time) error {
	for target, entry := range b.targets {
		if len(entry.pending) == 0 {
			continue
		}
		// 在持久化事务内重设基线，首次使用时也如此。
		// 期间可能有其他写入者提交了该队列的部分或全部。
		counted, err := probeWatermark(ctx, tx, nodeID, target)
		if err != nil {
			return err
		}
		for _, result := range entry.pending {
			if !result.CheckedAt.After(counted) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(node_id,target) DO UPDATE SET ok=excluded.ok,latency_ms=excluded.latency_ms,error=excluded.error,status_code=excluded.status_code,checked_at=excluded.checked_at WHERE excluded.checked_at>probe_latest.checked_at`, nodeID, target, result.OK, result.LatencyMS, result.Error, result.StatusCode, timekey.Format(result.CheckedAt)); err != nil {
				return err
			}
			counted = result.CheckedAt
			// 迟到的报告会更新最新水位线，但无法在保留策略删除后
			// 恢复已过期的按小时行。
			hour := result.CheckedAt.UTC().Truncate(time.Hour)
			if hour.Before(now.Add(-probeRetention)) {
				continue
			}
			failures := 0
			if !result.OK {
				failures = 1
			}
			latency := 0.0
			if result.LatencyMS != nil {
				latency = *result.LatencyMS
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES(?,?,?,1,?,?) ON CONFLICT(node_id,target,hour) DO UPDATE SET checks=probe_hourly.checks+1,failures=probe_hourly.failures+excluded.failures,latency_sum_ms=probe_hourly.latency_sum_ms+excluded.latency_sum_ms`, nodeID, target, timekey.Format(hour), failures, latency); err != nil {
				return err
			}
		}
		if counted.After(entry.counted) {
			entry.counted = counted
		}
		entry.pending = nil
		b.targets[target] = entry
	}
	return nil
}

func (state *nodeState) probesPending() bool {
	for _, entry := range state.probes.targets {
		if len(entry.pending) != 0 {
			return true
		}
	}
	return false
}
