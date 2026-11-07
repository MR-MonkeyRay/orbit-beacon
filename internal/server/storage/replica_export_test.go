package storage

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
)

// 仅测试链接可见：用真实源钩子和应用器验证 SQLite 作为后续权威时的反向编码。
func ReplicaTestConnect(ctx context.Context, source, target *Database, targetEndpoint config.DatabaseEndpoint) error {
	resource, instance, epoch := uuid.NewString(), uuid.NewString(), uuid.NewString()
	registered, err := hasReplica(ctx, source)
	if err != nil {
		return err
	}
	if registered {
		resource, instance, err = replicaIdentity(ctx, source)
		if err != nil {
			return err
		}
		p, err := position(ctx, source.current.reader)
		if err != nil {
			return err
		}
		epoch = p.Epoch
	}
	if err = installReplica(ctx, source, resource, instance, epoch, replicaProof{}); err != nil {
		return err
	}
	targetID := uuid.NewString()
	registered, err = hasReplica(ctx, target)
	if err != nil {
		return err
	}
	if registered {
		var existing string
		existing, targetID, err = replicaIdentity(ctx, target)
		if err != nil || existing != resource {
			return ErrIdentityMismatch
		}
	}
	if err = installReplica(ctx, target, resource, targetID, epoch, replicaProof{}); err != nil {
		return err
	}
	d := &replicaDomain{source: source, target: target, resource: resource, epoch: epoch, domain: source.domain}
	source.replica = d
	if _, err = source.current.writer.ExecContext(ctx, "UPDATE replica_state SET capture_mode='source' WHERE id=1"); err != nil {
		return err
	}
	if target.applyPool == nil {
		if err = openApplyPool(ctx, target, targetEndpoint); err != nil {
			return err
		}
	}
	return nil
}
func ReplicaTestDrain(ctx context.Context, source, target *Database) error {
	d := source.replica
	if d == nil || d.target != target {
		return errors.New("invalid test connection")
	}
	return d.catchUp(ctx)
}

func ReplicaTestSnapshotPinned(dbs *Databases, domain Domain) (int64, bool) {
	d := dbs.replication.domains[domain]
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pin, d.pinned
}
func ReplicaTestHoldSnapshot(ctx context.Context, dbs *Databases, domain Domain) (<-chan struct{}, func(), error) {
	d := dbs.replication.domains[domain]
	if err := d.control.revoke(ctx, domain, "rebuilding"); err != nil {
		return nil, nil, err
	}
	select {
	case d.source.writeGate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	locked := true
	defer func() {
		if locked {
			<-d.source.writeGate
		}
	}()
	readCtx, cancel := context.WithCancel(ctx)
	l, err := d.source.Acquire(readCtx, Reader)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	tx, err := l.BeginRead(l.Context())
	if err != nil {
		l.Close()
		cancel()
		return nil, nil, err
	}
	var seq int64
	if err = tx.QueryRowContext(l.Context(), "SELECT head_seq FROM replica_state WHERE id=1").Scan(&seq); err != nil {
		tx.Rollback()
		l.Close()
		cancel()
		return nil, nil, err
	}
	d.mu.Lock()
	d.seedBlocked.Store(false)
	d.pin = seq
	d.pinned = true
	d.cancelSeed = cancel
	d.mu.Unlock()
	locked = false
	<-d.source.writeGate
	cleanup := func() {
		cancel()
		tx.Rollback()
		l.Close()
		d.mu.Lock()
		d.pinned = false
		d.cancelSeed = nil
		d.mu.Unlock()
	}
	return readCtx.Done(), cleanup, nil
}
