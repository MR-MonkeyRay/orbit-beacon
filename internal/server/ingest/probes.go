package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/timekey"
)

const probeTargetLimit = 5

// At the admitted one-report/second rate this exceeds a normal 30s persistence
// interval, including bursts. Backpressure preserves accepted checks on overflow.
const probePendingLimit = 64
const probeRetention = 7 * 24 * time.Hour

type probeTargetBuffer struct {
	counted time.Time
	pending []v1.ProbeResult
}

type probeBuffer struct {
	credential [sha256.Size]byte
	generation uint64
	session    string
	targets    map[string]probeTargetBuffer
}

type probeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// The node's accept gate serializes buffer changes and commits. Identity is the
// durable identity observed when buffering, including across a process advance.
func (state *nodeState) fenceProbes(credential []byte, generation uint64, session string) {
	b := &state.probes
	if b.targets != nil && (subtle.ConstantTimeCompare(b.credential[:], credential) != 1 || b.generation != generation || b.session != session) {
		*b = probeBuffer{}
	}
}

func (state *nodeState) copyProbes(credential []byte, generation uint64, session string) probeBuffer {
	state.mu.Lock()
	defer state.mu.Unlock()
	b := probeBuffer{generation: generation, session: session, targets: make(map[string]probeTargetBuffer, probeTargetLimit)}
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

func (b *probeBuffer) buffer(ctx context.Context, q probeQueryer, nodeID string, results []v1.ProbeResult) error {
	// Reserve existing targets before evicting idle slots, so order within a
	// report cannot displace a target that the same report still contains.
	wanted := make(map[string]bool, len(results))
	for _, result := range results {
		wanted[result.Target] = true
	}
	for _, result := range results {
		entry, exists := b.targets[result.Target]
		if !exists {
			if len(b.targets) >= probeTargetLimit {
				oldest := ""
				for target, candidate := range b.targets {
					if wanted[target] || len(candidate.pending) != 0 {
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
		// Reports belong to their caller; take ownership of pointer values too.
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
		// Rebase inside the durable transaction as well as seeding on first use.
		// Another writer may have committed part or all of this queue meanwhile.
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
			// Late reports update the latest watermark but cannot resurrect expired
			// hourly rows after retention has removed them.
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
