package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"orbitbeacon/internal/config"
)

type replicaProbeKey struct{}

var errReplicaProbeBusy = errors.New("storage_replica_probe_busy")

type replicaDomain struct {
	source, target  *Database
	control         *replication
	resource, epoch string
	domain          Domain
	mu              sync.Mutex
	pin             int64
	pinned          bool
	cancelSeed      context.CancelFunc
	revoke          func(context.Context) error
	applied         atomic.Int64
	seedBlocked     atomic.Bool
}

func mysqlConnectorDB(cfg *mysql.Config) (*sql.DB, error) {
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, safeFailure(err)
	}
	return sql.OpenDB(connector), nil
}
func openApplyPool(ctx context.Context, db *Database, endpoint config.DatabaseEndpoint) error {
	if db.current.dialect != MySQL {
		return nil
	}
	cfg, err := mysqlConfig(endpoint, db.domain)
	if err != nil {
		return err
	}
	cfg.MultiStatements = true
	pool, err := mysqlConnectorDB(cfg)
	if err != nil {
		return err
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	if err = pool.PingContext(ctx); err != nil {
		pool.Close()
		return safeFailure(err)
	}
	db.applyPool = pool
	return nil
}
func (tx *Tx) beginCapture() error {
	r := tx.lease.db.replica
	if tx.receipt == nil {
		tx.receipt = &commitReceipt{id: uuid.NewString()}
	}
	var mode string
	var p replicaPosition
	if err := tx.tx.QueryRowContext(tx.ctx, "SELECT epoch_uuid,head_seq,head_tx_uuid,capture_mode FROM replica_state WHERE id=1").Scan(&p.Epoch, &p.Seq, &p.ID, &mode); err != nil {
		return safeFailure(err)
	}
	if mode == "off" {
		if pending, allowed := tx.lease.db.restoreAccess(tx.ctx); pending && allowed {
			return nil
		}
		return ErrRestorePending
	}
	if mode != "source" || p.Epoch != r.epoch {
		return ErrIdentityMismatch
	}
	boot, barrier, err := replicaClock()
	if err != nil {
		return err
	}
	id := tx.receipt.id
	b := &replicaTransaction{replicaPosition: replicaPosition{Epoch: p.Epoch, Seq: p.Seq + 1, ID: id}, Resource: r.resource, Previous: p.ID, Boot: boot, Barrier: barrier}
	if _, err = tx.tx.ExecContext(tx.ctx, "INSERT INTO replica_transactions VALUES(?,?,?,?,?,?,0,?)", b.Epoch, b.Seq, b.ID, b.Previous, b.Boot, b.Barrier, transactionOverhead); err != nil {
		return safeFailure(err)
	}
	owner := int64(0)
	if tx.Dialect() == MySQL {
		if err = tx.tx.QueryRowContext(tx.ctx, "SELECT CONNECTION_ID()").Scan(&owner); err != nil {
			return safeFailure(err)
		}
	}
	if _, err = tx.tx.ExecContext(tx.ctx, "UPDATE replica_state SET open_tx_uuid=?,next_ordinal=0,owner_connection=? WHERE id=1", id, owner); err != nil {
		return safeFailure(err)
	}
	tx.capture = b
	return nil
}
func (tx *Tx) finishCapture() error {
	if err := tx.capturedError(); err != nil {
		return err
	}
	b := tx.capture
	var journal int64
	if err := tx.tx.QueryRowContext(tx.ctx, "SELECT t.change_count,t.payload_bytes,s.journal_bytes FROM replica_transactions t JOIN replica_state s ON s.id=1 WHERE t.tx_uuid=?", b.ID).Scan(&b.Count, &b.Bytes, &journal); err != nil {
		return safeFailure(err)
	}
	if b.Bytes > maxTransactionBytes {
		return ErrTransactionTooLarge
	}
	if journal+b.Bytes > maxJournalBytes {
		if err := tx.lease.db.replica.pruneTx(tx.ctx, tx.tx, time.Now(), b.Bytes); err != nil {
			return err
		}
	}
	_, err := tx.tx.ExecContext(tx.ctx, "UPDATE replica_state SET head_seq=?,head_tx_uuid=?,open_tx_uuid='',next_ordinal=0,owner_connection=0,journal_bytes=journal_bytes+? WHERE id=1", b.Seq, b.ID, b.Bytes)
	return safeFailure(err)
}

func (dbs *Databases) StartReplication(ctx context.Context) error {
	if dbs == nil || dbs.replication == nil {
		return nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return nil
	}
	if r.record.Pending != "" || r.record.Disabled {
		return ErrRestorePending
	}
	run, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	var wg sync.WaitGroup
	for _, d := range r.domains {
		if d.target == nil {
			continue
		}
		wg.Add(1)
		go func(d *replicaDomain) { defer wg.Done(); d.run(run) }(d)
	}
	go func() { wg.Wait(); close(r.done) }()
	return nil
}
func (dbs *Databases) StopReplication() error {
	if dbs == nil || dbs.replication == nil {
		return nil
	}
	r := dbs.replication
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	if cancel != nil {
		cancel()
	}
	r.mu.Unlock()
	if done != nil {
		<-done
	}
	r.mu.Lock()
	if r.done == done {
		r.cancel = nil
		r.done = nil
	}
	r.mu.Unlock()
	return nil
}
func (d *replicaDomain) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := d.step(ctx); err != nil {
			if errors.Is(err, ErrIdentityMismatch) || errors.Is(err, ErrReplicaDiverged) || errors.Is(err, ErrSchemaIncompatible) || errors.Is(err, ErrTransactionTooLarge) {
				if d.control != nil {
					r := d.control
					r.mu.Lock()
					m := r.record.Domains[d.domain]
					gap := errors.Is(err, ErrReplicaDiverged) && !errors.Is(err, ErrIdentityMismatch) && !errors.Is(err, ErrSchemaIncompatible) && !errors.Is(err, ErrTransactionTooLarge)
					// GC 已撤销资格时，旧追赶快照的缺洞不得覆盖可重建决定。
					if m != nil && (m.Ready || m.State != "error") && !(gap && !m.Ready && m.State == "rebuild_required") {
						old := *m
						m.Ready, m.State = false, "error"
						r.record.Revision++
						if persistErr := r.persistLocked(ctx); persistErr != nil {
							*m = old
							r.record.Revision--
						}
					}
					r.mu.Unlock()
				}
			}
		}
		timer.Reset(time.Second)
	}
}
func (d *replicaDomain) step(ctx context.Context) error {
	if err := d.barrier(ctx); err != nil {
		return err
	}
	d.control.mu.Lock()
	member := *d.control.record.Domains[d.domain]
	d.control.mu.Unlock()
	if member.State == "error" {
		return d.prune(ctx, time.Now())
	}
	var copyErr error
	if !member.Ready {
		copyErr = d.seed(ctx)
	} else {
		copyErr = d.catchUp(ctx)
	}
	pruneErr := d.prune(ctx, time.Now())
	return errors.Join(copyErr, pruneErr)
}
func (d *replicaDomain) barrier(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	probe = context.WithValue(probe, replicaProbeKey{}, true)
	boot, now, err := replicaClock()
	if err != nil {
		return err
	}
	var previousBoot, previousID string
	var previous int64
	err = d.source.current.reader.QueryRowContext(probe, "SELECT t.tx_uuid,t.boot_id,t.barrier_ns FROM replica_state s JOIN replica_transactions t ON t.epoch_uuid=s.epoch_uuid AND t.seq=s.head_seq WHERE s.id=1").Scan(&previousID, &previousBoot, &previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return safeFailure(err)
	}
	if previousBoot == boot && now-previous < int64(time.Second) {
		return nil
	}
	l, err := d.source.Acquire(probe, Writer)
	if err != nil {
		return err
	}
	defer l.Close()
	tx, err := l.BeginWrite(probe)
	if errors.Is(err, errReplicaProbeBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if tx.capture.Previous != "" && tx.capture.Previous != previousID {
		err = tx.tx.QueryRowContext(probe, "SELECT boot_id,barrier_ns FROM replica_transactions WHERE epoch_uuid=? AND seq=?", tx.capture.Epoch, tx.capture.Seq-1).Scan(&previousBoot, &previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return safeFailure(err)
		}
		if err == nil && previousBoot == boot && tx.capture.Barrier-previous < int64(time.Second) {
			return nil
		}
	}
	err = tx.Commit()
	if errors.Is(err, ErrCommitUnknown) {
		l.Close()
		_, err = tx.ResolveCommit(probe)
	}
	return err
}
func (d *replicaDomain) catchUp(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := position(ctx, d.target.current.reader)
		if err != nil {
			return err
		}
		d.applied.Store(target.Seq)
		source, err := position(ctx, d.source.current.reader)
		if err != nil {
			return err
		}
		if target == source {
			return nil
		}
		if target.Epoch != source.Epoch || target.Seq > source.Seq {
			return ErrReplicaDiverged
		}
		batch, err := readReplicaTransaction(ctx, d.source, target.Seq)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrReplicaDiverged
			}
			return err
		}
		if err = applyReplicaTransaction(ctx, d.target, batch); err != nil {
			return err
		}
		d.applied.Store(batch.Seq)
	}
}
func (d *replicaDomain) prune(ctx context.Context, now time.Time) error {
	select {
	case d.source.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
	defer func() { <-d.source.writeGate }()
	if err := d.source.resolvePending(ctx); err != nil {
		return err
	}
	tx, err := d.source.current.writer.BeginTx(ctx, nil)
	if err != nil {
		return safeFailure(err)
	}
	defer tx.Rollback()
	if d.source.current.dialect == MySQL {
		var id string
		if err = tx.QueryRowContext(ctx, "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1 FOR UPDATE").Scan(&id); err != nil {
			return safeFailure(err)
		}
	}
	if err = d.pruneTx(ctx, tx, now, 0); err != nil {
		return err
	}
	return safeFailure(tx.Commit())
}
func (d *replicaDomain) pruneTx(ctx context.Context, tx *sql.Tx, now time.Time, incoming int64) error {
	// 墙钟只用于保留期；事务屏障的 RPO 仍使用 boot+boottime。
	var total, head int64
	var epoch string
	if err := tx.QueryRowContext(ctx, "SELECT epoch_uuid,journal_bytes,head_seq FROM replica_state WHERE id=1").Scan(&epoch, &total, &head); err != nil {
		return safeFailure(err)
	}
	boot, current, err := replicaClock()
	if err != nil {
		return err
	}
	age := int64(time.Duration(journalRetentionHours) * time.Hour)
	elapsed := time.Since(now)
	if elapsed < 0 {
		current -= int64(elapsed)
	}
	var removed, cursor, lastRemoved, count int64
	stop := false
	for !stop {
		rows, err := tx.QueryContext(ctx, "SELECT seq,payload_bytes,boot_id,barrier_ns FROM replica_transactions WHERE epoch_uuid=? AND seq>? AND seq<? ORDER BY seq LIMIT 256", epoch, cursor, head)
		if err != nil {
			return safeFailure(err)
		}
		seen := 0
		for rows.Next() {
			var seq, size, barrier int64
			var b string
			if err = rows.Scan(&seq, &size, &b, &barrier); err != nil {
				rows.Close()
				return safeFailure(err)
			}
			seen++
			expired := b != boot || current-barrier > age
			if !expired && total+incoming-removed <= maxJournalBytes {
				stop = true
				break
			}
			lastRemoved = seq
			count++
			removed += size
			cursor = seq
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return safeFailure(err)
		}
		if seen < 256 || incoming == 0 {
			stop = true
		}
	}
	if count == 0 {
		if total+incoming > maxJournalBytes {
			return ErrTransactionTooLarge
		}
		return nil
	}
	// 先持久撤销资格，再取消快照 pin，最后才删除旧事务。
	d.mu.Lock()
	pinLost := d.pinned && lastRemoved > d.pin
	d.mu.Unlock()
	if pinLost {
		d.seedBlocked.Store(true)
	}
	needed := lastRemoved > d.applied.Load()
	if (needed || pinLost) && d.revoke != nil {
		if err = d.revoke(ctx); err != nil {
			return err
		}
	}
	d.mu.Lock()
	if pinLost {
		if d.cancelSeed != nil {
			d.cancelSeed()
		}
		d.pinned = false
	}
	d.mu.Unlock()
	if _, err = tx.ExecContext(ctx, "DELETE FROM replica_transactions WHERE epoch_uuid=? AND seq<=?", epoch, lastRemoved); err != nil {
		return safeFailure(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE replica_state SET retained_from_seq=?,journal_bytes=journal_bytes-? WHERE id=1", lastRemoved+1, removed); err != nil {
		return safeFailure(err)
	}
	if total+incoming-removed > maxJournalBytes {
		return ErrTransactionTooLarge
	}
	return nil
}
