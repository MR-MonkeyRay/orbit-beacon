package storage

import (
	"context"
	"errors"
	"time"
)

// possibleCommit 是本进程在源 guard 下观察到的完整前驱，或最后一次可能发出 COMMIT 的位置。
type possibleCommit struct {
	position replicaPosition
	boot     string
	valid    bool
}

type promotionProof struct {
	position  replicaPosition
	lossNanos int64
}

func (db *Database) notePossibleCommit(boot string, p replicaPosition) {
	db.mu.Lock()
	db.possible = possibleCommit{position: p, boot: boot, valid: true}
	db.mu.Unlock()
}

func (d *replicaDomain) drainFailure(ctx context.Context, err error) error {
	// 停进程不是排空超时，不得因此永久锁住下一次恢复。
	if errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	if persistErr := d.update(context.Background(), func(m *memberRecord) { m.Manual = true; m.State = "manual_required"; m.Reason = "drain_timeout" }); persistErr != nil {
		return persistErr
	}
	return err
}

func (d *replicaDomain) includesAllPossible(ctx context.Context, p replicaPosition, boot string) (bool, error) {
	if err := d.drainForSwitch(ctx); err != nil {
		return false, d.drainFailure(ctx, err)
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	db := d.source
	select {
	case db.writeGate <- struct{}{}:
	case <-probe.Done():
		return false, probe.Err()
	}
	defer func() { <-db.writeGate }()
	db.mu.Lock()
	defer db.mu.Unlock()
	bound := db.possible
	if db.closed || db.current == nil || db.current.refs != 0 || db.admitting || db.current.ctx.Err() == nil || !bound.valid || bound.boot != boot || bound.position != p {
		return false, nil
	}
	// 不能把未知提交的上界当已回滚；只有候选也包含该 UUID 才覆盖所有可能结果。
	if db.pending != nil && (db.pending.position == nil || *db.pending.position != p || db.pending.id != p.ID) {
		return false, nil
	}
	return true, nil
}
