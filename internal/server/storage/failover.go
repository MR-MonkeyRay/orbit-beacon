package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
)

// SwitchHooks 的 Publish 只接收已绑定候选的租约，不经过关闭中的公共准入。
type SwitchHooks struct {
	Prepare func(context.Context) error
	Publish func(context.Context, *Lease) error
}

func (dbs *Databases) SetMainSwitchHooks(hooks SwitchHooks) {
	if dbs == nil || dbs.Main == nil {
		return
	}
	dbs.Main.mu.Lock()
	dbs.Main.hooks = hooks
	dbs.Main.mu.Unlock()
}

type freezePoint struct {
	Boot  string
	Nanos int64
	Wall  time.Time
}

type switchIntent struct {
	Source, Target, SourceInstance, TargetInstance string
	Epoch                                          string
	Checkpoint                                     replicaPosition
	Generation                                     uint64
	Pointer                                        bool
}

type rebuildOperation struct {
	ID, Source, Target, Epoch, Phase string
	Checkpoint                       replicaPosition
}

type promotionError struct {
	phase string
	cause error
}

func (e *promotionError) Error() string { return "storage_" + e.phase }
func (e *promotionError) Unwrap() error { return e.cause }
func promotionFailure(phase string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		phase += "_timeout"
	}
	return &promotionError{phase: phase, cause: err}
}

func storageFailure(err error) bool {
	return err != nil && (errors.Is(err, ErrCommitUnknown) || IsConnectionFailure(err))
}

func (r *replication) now() (freezePoint, error) {
	if r != nil && r.clock != nil {
		b, n, w, e := r.clock()
		return freezePoint{b, n, w}, e
	}
	b, n, e := replicaClock()
	return freezePoint{b, n, time.Now().UTC()}, e
}
func (r *replication) failoverConfig() config.FailoverConfig {
	f := r.cfg.Failover
	if f.FailureTimeout == 0 {
		f.FailureTimeout = 30 * time.Second
		f.MaxLossWindow = 10 * time.Second
	}
	if f.RecoveryStable == 0 {
		f.RecoveryStable = 60 * time.Second
	}
	if f.ManualAfter == 0 {
		f.ManualAfter = 24 * time.Hour
	}
	return f
}

func (d *replicaDomain) member() memberRecord {
	d.control.mu.Lock()
	defer d.control.mu.Unlock()
	return *d.control.record.Domains[d.domain]
}
func (d *replicaDomain) update(ctx context.Context, fn func(*memberRecord)) error {
	r := d.control
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[d.domain]
	old := *m
	fn(m)
	r.record.Revision++
	if err := r.persistLocked(ctx); err != nil {
		*m = old
		r.record.Revision--
		return err
	}
	return nil
}

// freeze 不等待 SQL、write gate 或租约；本地决定失败时保持关闭。
func (d *replicaDomain) freeze(ctx context.Context, reason string) error {
	db := d.source
	db.mu.Lock()
	db.admitting = false
	db.writeFailed = true
	if db.current != nil {
		db.current.cancel()
	}
	db.mu.Unlock()
	if d.control == nil {
		return nil
	}
	f, err := d.control.now()
	if err != nil {
		return err
	}
	r := d.control
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[d.domain]
	if m == nil {
		return ErrIdentityMismatch
	}
	if m.Freeze != nil {
		return nil
	}
	old := *m
	m.Freeze = &f
	if m.Active == "primary" {
		m.PrimaryReachable = false
	} else {
		m.StandbyReachable = false
	}
	if m.FailureSince.IsZero() {
		m.FailureSince = f.Wall
	}
	m.StableSince = nil
	m.Reason = reason
	if m.Manual {
		m.State = "manual_required"
	} else {
		m.State = "suspect"
	}
	r.record.Revision++
	if err = r.persistLocked(ctx); err != nil {
		*m = old
		r.record.Revision--
		return err
	}
	return nil
}

func (db *Database) privateLease(ctx context.Context, pool Pool) (*Lease, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed || db.current == nil || db.current.writer == nil {
		return nil, ErrUnavailable
	}
	// 探测可在冻结后解析 guard，但不能借用已取消的业务 context。
	gc, cancel := context.WithCancel(ctx)
	idle := make(chan struct{})
	close(idle)
	g := &generation{id: db.current.id, dialect: db.current.dialect, writer: db.current.writer, reader: db.current.reader, maintenanceConnector: db.current.maintenanceConnector, ctx: gc, cancel: cancel, idle: idle}
	lc, stopCancel := context.WithCancel(gc)
	l := &Lease{db: db, gen: g, pool: pool, cancel: func() { stopCancel(); cancel() }, stop: func() bool { return true }, refs: 1}
	l.ctx = context.WithValue(lc, leaseKey{}, &leaseBinding{lease: l})
	db.retainLocked(g)
	return l, nil
}

func unavailableDatabase(domain Domain) *Database {
	db := &Database{domain: domain, writeGate: make(chan struct{}, 1)}
	return db
}

func (d *replicaDomain) endpoint(role string) (config.DatabaseEndpoint, string, bool) {
	m := d.member()
	if role == "primary" {
		return d.control.cfg.Primary, m.Primary, m.PrimaryRegistered
	}
	return *d.control.cfg.Standby, m.Standby, m.StandbyRegistered
}
func opposite(role string) string {
	if role == "primary" {
		return "standby"
	}
	return "primary"
}

func validateExistingReplica(ctx context.Context, db *Database, resource, instance string, proof replicaProof) error {
	phase, cancel := context.WithTimeout(ctx, 2*time.Second)
	actual, id, err := replicaIdentity(phase, db)
	cancel()
	if err != nil {
		return err
	}
	if actual != resource || id != instance {
		return ErrIdentityMismatch
	}
	if err = validateReplicaSchema(ctx, db); err != nil {
		return promotionFailure("candidate_schema", err)
	}
	if err = validateProtocolDefinitions(ctx, db); err != nil {
		return promotionFailure("candidate_definitions", err)
	}
	if err = validateCaptureTriggers(ctx, db); err != nil {
		return promotionFailure("candidate_triggers", err)
	}
	phase, cancel = context.WithTimeout(ctx, 2*time.Second)
	if proof.Rebuilding {
		// 仅持久授权的 inactive 重建可处于半份水位；身份、全部定义已先核对。
		err = db.offlineGuard(phase, func(tx *sql.Tx) error {
			var actual, id, domain string
			var version int
			if err := tx.QueryRowContext(phase, "SELECT resource_uuid,instance_uuid,domain,schema_version FROM replica_identity WHERE id=1").Scan(&actual, &id, &domain, &version); err != nil {
				return safeFailure(err)
			}
			expected := CurrentSchemaVersion
			if db.domain == HistoryDomain {
				expected = HistorySchemaVersion
			}
			if actual != resource || id != instance || domain != string(db.domain) || version != expected {
				return ErrIdentityMismatch
			}
			return nil
		})
	} else {
		err = validateProtocolState(phase, db, proof)
	}
	cancel()
	return err
}

func (d *replicaDomain) openMember(ctx context.Context, role string) (*Database, error) {
	m := d.member()
	endpoint, id, registered := d.endpoint(role)
	if role != m.Active && m.Manual && !registered {
		return nil, ErrUnavailable
	}
	if registered {
		phase, cancel := context.WithTimeout(ctx, 2*time.Second)
		resource, instance, err := inspectMember(phase, endpoint, d.domain, d.control.dataDir)
		cancel()
		if err != nil {
			return nil, err
		}
		if resource != m.Resource || instance != id {
			return nil, ErrIdentityMismatch
		}
	}
	activeID := m.Primary
	if m.Active == "standby" {
		activeID = m.Standby
	}
	rebuilding := false
	if role != m.Active && m.Rebuild != nil {
		op := m.Rebuild
		rebuilding = registered && !m.Ready && replicaUUID(op.ID) && op.Source == activeID && op.Target == id && op.Epoch == m.Epoch && op.Checkpoint.Epoch == m.Epoch && (op.Phase == "clearing" || op.Phase == "copying" || op.Phase == "catching_up")
		if !rebuilding {
			return nil, ErrIdentityMismatch
		}
	}
	proof := replicaProof{Checkpoint: m.checkpointFor(id), Ready: role != m.Active && m.Ready, Rebuilding: rebuilding}
	var db *Database
	var err error
	if registered {
		phase, cancel := context.WithTimeout(ctx, 2*time.Second)
		db, err = OpenDomain(phase, endpoint, d.domain, d.control.dataDir)
		cancel()
		if err == nil {
			err = validateExistingReplica(ctx, db, m.Resource, id, proof)
		}
	} else if role != m.Active {
		db, err = openReplicaTarget(ctx, endpoint, d.domain, d.control.dataDir, m.Resource, id, m.Epoch, proof)
	} else {
		d.control.mu.Lock()
		initial := role == "primary" && d.control.record.BootstrapPending && !d.control.record.Bootstrapped
		d.control.mu.Unlock()
		if !initial {
			return nil, ErrIdentityMismatch
		}
		db, err = OpenDomain(ctx, endpoint, d.domain, d.control.dataDir)
		if err == nil {
			err = installReplica(ctx, db, m.Resource, id, m.Epoch, proof)
		}
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		return nil, err
	}
	if role == m.Active {
		phase, cancel := context.WithTimeout(ctx, 2*time.Second)
		p, positionErr := position(phase, db.current.reader)
		cancel()
		if positionErr != nil || p.Epoch != m.Epoch {
			db.Close()
			if positionErr != nil {
				return nil, positionErr
			}
			return nil, ErrIdentityMismatch
		}
	} else {
		phase, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = openApplyPool(phase, db, endpoint)
		cancel()
		if err != nil {
			db.Close()
			return nil, err
		}
		if m.Ready {
			phase, cancel := context.WithTimeout(ctx, 2*time.Second)
			p, positionErr := position(phase, db.current.reader)
			cancel()
			if positionErr != nil {
				db.Close()
				return nil, positionErr
			}
			if p.Epoch == m.Epoch {
				d.applied.Store(p.Seq)
			}
		}
	}
	if !registered {
		if err = d.update(ctx, func(m *memberRecord) {
			if role == "primary" {
				m.PrimaryRegistered = true
			} else {
				m.StandbyRegistered = true
			}
		}); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func (d *replicaDomain) startupFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	integrity := !storageFailure(err) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrUnavailable)
	reason := "active_unreachable"
	if integrity {
		reason = "active_integrity_error"
	}
	if err := d.freeze(ctx, reason); err != nil {
		return err
	}
	if integrity {
		return d.update(ctx, func(m *memberRecord) {
			m.Ready = false
			m.Manual = true
			m.State = "manual_required"
			m.Reason = reason
		})
	}
	return nil
}

func (r *replication) openRegistered(ctx context.Context, dbs *Databases, domains []Domain) error {
	if r.record.Pending != "" {
		return ErrRestorePending
	}
	for _, domain := range domains {
		m := r.record.Domains[domain]
		if m == nil || (m.Active != "primary" && m.Active != "standby") {
			return ErrIdentityMismatch
		}
		if m.Generation == 0 {
			m.Generation = 1
		}
		source := unavailableDatabase(domain)
		d := &replicaDomain{source: source, control: r, resource: m.Resource, epoch: m.Epoch, domain: domain}
		d.revoke = func(ctx context.Context) error { return r.revoke(ctx, domain, "rebuild_required") }
		source.replica = d
		r.domains[domain] = d
		setDomain(dbs, domain, source)
		// pending intent 先保持关闭；run 核对候选 marker，不能先回到配置主库。
		if m.Intent != nil {
			continue
		}
		active, err := d.openMember(ctx, m.Active)
		if err != nil {
			if err = d.startupFailure(ctx, err); err != nil {
				return err
			}
		} else {
			source.current = active.current
			if m.Generation == 0 {
				m.Generation = 1
			}
			source.current.id = m.Generation
			if m.Freeze == nil {
				phase, cancel := context.WithTimeout(ctx, 2*time.Second)
				err = source.offlineGuard(phase, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(phase, "UPDATE replica_state SET capture_mode='source' WHERE id=1")
					return err
				})
				cancel()
				if err != nil {
					if err = d.startupFailure(ctx, err); err != nil {
						return err
					}
				} else {
					source.admitting = true
				}
			} else {
				source.current.cancel()
			}
		}
		// 未持久 F 的重启必须从本次关闭准入的时刻建立保守边界。
		if source.current == nil && m.Freeze == nil {
			if err = d.freeze(ctx, "active_unreachable"); err != nil {
				return err
			}
		}
	}
	return r.persistLocked(ctx)
}

func (d *replicaDomain) checkManual(ctx context.Context) error {
	m := d.member()
	if m.Manual || m.FailureSince.IsZero() {
		return nil
	}
	now, err := d.control.now()
	if err != nil {
		return err
	}
	f := d.control.failoverConfig()
	if now.Wall.Before(m.FailureSince) || now.Wall.Sub(m.FailureSince) >= f.ManualAfter {
		return d.update(ctx, func(m *memberRecord) {
			m.Manual = true
			m.State = "manual_required"
			m.Reason = "recovery_deadline_exceeded"
		})
	}
	return nil
}

func (d *replicaDomain) promotionCheckpoint(ctx context.Context, m memberRecord) (promotionProof, error) {
	if d.target == nil || !m.Ready || m.manualBlocked() || m.Freeze == nil {
		return promotionProof{}, ErrUnavailable
	}
	f := m.Freeze
	now, err := d.control.now()
	if err != nil {
		return promotionProof{}, err
	}
	if now.Boot != f.Boot || now.Nanos < f.Nanos {
		return promotionProof{}, ErrUnavailable
	}
	_, id, _ := d.endpoint(opposite(m.Active))
	if err = validateExistingReplica(ctx, d.target, m.Resource, id, replicaProof{Checkpoint: m.checkpointFor(id), Ready: true}); err != nil {
		return promotionProof{}, err
	}
	// 完整只读校验之后，重新取得候选的最新位置与屏障。
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	var p replicaPosition
	var bound int64
	var temporal error
	stage := "candidate_guard"
	err = d.target.offlineGuard(probe, func(tx *sql.Tx) error {
		resource, instance, err := replicaIdentity(probe, d.target)
		if err != nil {
			return err
		}
		if resource != m.Resource || instance != id {
			return ErrIdentityMismatch
		}
		var ready int
		if err = tx.QueryRowContext(probe, "SELECT ready FROM replica_state WHERE id=1").Scan(&ready); err != nil {
			return err
		}
		if ready != 1 {
			stage = "candidate_incomplete"
			return ErrUnavailable
		}
		p, err = position(probe, tx)
		if err != nil {
			return err
		}
		if p.Epoch != m.Epoch {
			stage = "candidate_epoch_mismatch"
			return ErrReplicaDiverged
		}
		var boot string
		var barrier int64
		err = tx.QueryRowContext(probe, "SELECT boot_id,barrier_ns FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=?", p.Epoch, p.Seq, p.ID).Scan(&boot, &barrier)
		if errors.Is(err, sql.ErrNoRows) {
			temporal = promotionFailure("candidate_barrier_missing", ErrUnavailable)
			return nil
		}
		if err != nil {
			return err
		}
		if boot != f.Boot {
			temporal = promotionFailure("candidate_boot_mismatch", ErrUnavailable)
			return nil
		}
		if barrier > f.Nanos || barrier < 0 {
			temporal = promotionFailure("candidate_clock_unproven", ErrUnavailable)
			return nil
		}
		bound = f.Nanos - barrier
		if bound > int64(d.control.failoverConfig().MaxLossWindow) {
			temporal = promotionFailure("replica_lag_exceeded", ErrUnavailable)
		}
		return nil
	})
	cancel()
	if err != nil {
		return promotionProof{}, promotionFailure(stage, err)
	}
	if temporal == nil {
		return promotionProof{position: p, lossNanos: bound}, nil
	}
	covered, err := d.includesAllPossible(ctx, p, now.Boot)
	if err != nil {
		return promotionProof{}, err
	}
	if covered {
		return promotionProof{position: p, lossNanos: 0}, nil
	}
	return promotionProof{}, temporal
}

func freshGeneration(candidate *generation, id uint64) *generation {
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &generation{id: id, dialect: candidate.dialect, writer: candidate.writer, reader: candidate.reader, maintenanceConnector: candidate.maintenanceConnector, ctx: ctx, cancel: cancel, idle: idle}
}

func (d *replicaDomain) drainForSwitch(ctx context.Context) error {
	drain, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db := d.source
	db.mu.Lock()
	hooks := db.hooks
	db.mu.Unlock()
	if hooks.Prepare != nil {
		if err := hooks.Prepare(drain); err != nil {
			return err
		}
	}
	return db.drain(drain)
}
func (d *replicaDomain) publish(ctx context.Context, id uint64) error {
	db := d.source
	db.mu.Lock()
	if db.current == nil {
		db.mu.Unlock()
		return ErrUnavailable
	}
	if db.current.ctx.Err() != nil || db.current.id != id {
		db.current = freshGeneration(db.current, id)
	}
	hooks := db.hooks
	db.admitting = false
	db.mu.Unlock()
	if hooks.Publish != nil {
		lease, err := db.privateLease(ctx, Reader)
		if err != nil {
			return err
		}
		err = hooks.Publish(lease.Context(), lease)
		lease.Close()
		if err != nil {
			return err
		}
	}
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return ErrUnavailable
	}
	db.writeFailed = false
	db.admitting = true
	db.mu.Unlock()
	return nil
}

func (d *replicaDomain) switchTo(ctx context.Context, role string, p replicaPosition) error {
	m := d.member()
	if m.Intent == nil {
		if err := d.checkManual(ctx); err != nil {
			return err
		}
		if d.member().manualBlocked() {
			return ErrUnavailable
		}
		if err := d.drainForSwitch(ctx); err != nil {
			return d.drainFailure(ctx, err)
		}
		if err := d.checkManual(ctx); err != nil {
			return err
		}
		if d.member().manualBlocked() {
			return ErrUnavailable
		}
		_, source, _ := d.endpoint(m.Active)
		_, target, _ := d.endpoint(role)
		generation := d.source.Generation() + 1
		if generation <= m.Generation {
			generation = m.Generation + 1
		}
		intent := &switchIntent{Source: m.Active, Target: role, SourceInstance: source, TargetInstance: target, Epoch: uuid.NewString(), Checkpoint: p, Generation: generation}
		if err := d.update(ctx, func(m *memberRecord) { m.Intent = intent }); err != nil {
			return err
		}
	}
	return d.finishSwitch(ctx)
}

func (d *replicaDomain) finishSwitch(ctx context.Context) error {
	m := d.member()
	intent := m.Intent
	if intent == nil {
		return nil
	}
	candidate := d.target
	if intent.Pointer && d.source.current != nil {
		candidate = d.source
	} else if candidate == nil {
		// intent 中的实例是唯一候选；重启不根据 endpoint 名称猜权威。
		endpoint, expected, _ := d.endpoint(intent.Target)
		if expected != intent.TargetInstance {
			return ErrIdentityMismatch
		}
		resource, instance, err := inspectMember(ctx, endpoint, d.domain, d.control.dataDir)
		if err != nil {
			return err
		}
		if resource != m.Resource || instance != expected {
			return ErrIdentityMismatch
		}
		candidate, err = OpenDomain(ctx, endpoint, d.domain, d.control.dataDir)
		if err != nil {
			return err
		}
		if err = validateReplicaSchema(ctx, candidate); err != nil {
			candidate.Close()
			return err
		}
		d.target = candidate
	}
	if candidate == nil {
		return ErrUnavailable
	}
	if err := validateExistingReplica(ctx, candidate, m.Resource, intent.TargetInstance, replicaProof{Checkpoint: &intent.Checkpoint, Ready: true}); err != nil {
		return err
	}
	// 重试 COMMIT 未知时，guard 后的 epoch marker 区分已完成和未完成。
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := candidate.offlineGuard(probe, func(tx *sql.Tx) error {
		resource, instance, err := replicaIdentity(probe, candidate)
		if err != nil || resource != m.Resource || instance != intent.TargetInstance {
			return ErrIdentityMismatch
		}
		current, err := position(probe, tx)
		if err != nil {
			return err
		}
		if current.Epoch == intent.Epoch {
			return nil
		}
		if current != intent.Checkpoint {
			return ErrReplicaDiverged
		}
		if _, err = tx.ExecContext(probe, "INSERT INTO replica_epochs VALUES(?,?,?,?)", intent.Epoch, current.Epoch, current.Seq, current.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(probe, "UPDATE replica_state SET epoch_uuid=?,head_seq=0,head_tx_uuid='',retained_from_seq=1,capture_mode='source',ready=1 WHERE id=1", intent.Epoch)
		return err
	}); err != nil {
		return err
	}
	if !intent.Pointer {
		next := *intent
		next.Pointer = true
		if err := d.update(ctx, func(m *memberRecord) {
			m.Active = next.Target
			if next.Target == "primary" {
				m.PrimaryReachable = true
			} else {
				m.StandbyReachable = true
			}
			m.Epoch = next.Epoch
			m.Generation = next.Generation
			m.Ready = false
			m.Checkpoint = nil
			m.Intent = &next
			m.StableSince = nil
			m.State = "resyncing"
		}); err != nil {
			return err
		}
		intent = &next
	}
	if candidate != d.source {
		db := d.source
		db.mu.Lock()
		old := db.current
		oldApply := db.applyPool
		db.current = freshGeneration(candidate.current, intent.Generation)
		db.applyPool = nil
		db.admitting = false
		apply := candidate.applyPool
		db.mu.Unlock()
		candidate.current = old
		candidate.applyPool = oldApply
		candidate.replica = nil
		if apply != nil {
			if err := apply.Close(); err != nil {
				return err
			}
		}
		d.target = candidate
		// 原进程的未知提交按候选已保留前缀解析，不能读回旧分支收据。
		if err := db.resolveSelectedReceipt(ctx, intent.Checkpoint); err != nil {
			return err
		}
	}
	d.epoch = intent.Epoch
	d.applied.Store(0)
	if err := d.publish(ctx, intent.Generation); err != nil {
		return err
	}
	return d.update(ctx, func(m *memberRecord) {
		m.Intent = nil
		m.Freeze = nil
		if m.Active == "primary" {
			m.FailureSince = time.Time{}
			m.LossNanos = nil
		}
		if m.Manual {
			m.State = "manual_required"
		} else if m.Active == "standby" {
			m.State = "standby_active"
		} else {
			m.State = "healthy"
		}
	})
}

func (db *Database) resolveSelectedReceipt(ctx context.Context, p replicaPosition) error {
	db.mu.Lock()
	pending := db.pending
	db.mu.Unlock()
	if pending == nil {
		return nil
	}
	var count int
	if err := db.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE tx_uuid=?", pending.id).Scan(&count); err != nil {
		return safeFailure(err)
	}
	committed := count == 1 || p.ID == pending.id
	if pending.position != nil && pending.position.Epoch == p.Epoch && pending.position.Seq < p.Seq {
		committed = true
	}
	pending.mu.Lock()
	pending.known = true
	pending.committed = committed
	pending.mu.Unlock()
	db.mu.Lock()
	if db.pending == pending {
		db.pending = nil
	}
	db.mu.Unlock()
	return nil
}

// coordinatorStep 由每域唯一 worker 串行调用；域之间不等待彼此的网络或排空。
func (d *replicaDomain) coordinatorAutomatic(ctx context.Context) error {
	if d.control == nil {
		return d.step(ctx)
	}
	m := d.member()
	if m.Intent != nil {
		return d.finishSwitch(ctx)
	}
	if err := d.checkManual(ctx); err != nil {
		return err
	}
	m = d.member()
	if d.source.current == nil {
		db, err := d.openMember(ctx, m.Active)
		if err == nil {
			d.source.mu.Lock()
			d.source.current = db.current
			d.source.current.id = m.Generation
			d.source.admitting = false
			d.source.mu.Unlock()
		} else if !storageFailure(err) && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	var healthErr error
	if d.source.current == nil {
		healthErr = ErrUnavailable
	} else {
		healthErr = d.barrier(ctx)
	}
	if errors.Is(healthErr, errReplicaProbeBusy) || IsBusy(healthErr) {
		return nil
	}
	if healthErr != nil {
		if !storageFailure(healthErr) && !errors.Is(healthErr, context.DeadlineExceeded) && !errors.Is(healthErr, ErrUnavailable) {
			return healthErr
		}
		if err := d.freeze(ctx, "active_unreachable"); err != nil {
			return err
		}
		return d.tryPromotion(ctx)
	}
	m = d.member()
	if m.Freeze != nil || !d.source.Ready() {
		if err := d.drainForSwitch(ctx); err != nil {
			return d.drainFailure(ctx, err)
		}
		generation := d.source.Generation() + 1
		if generation <= m.Generation {
			generation = m.Generation + 1
		}
		if err := d.update(ctx, func(m *memberRecord) { m.Generation = generation }); err != nil {
			return err
		}
		if err := d.publish(ctx, generation); err != nil {
			return err
		}
		if err := d.update(ctx, func(m *memberRecord) {
			m.Freeze = nil
			if m.Manual {
				m.State = "manual_required"
			} else if m.Active == "standby" {
				m.State = "standby_active"
			} else {
				m.State = "healthy"
				m.FailureSince = time.Time{}
			}
		}); err != nil {
			return err
		}
	}
	m = d.member()
	var targetErr error
	registered := m.StandbyRegistered
	if m.Active == "standby" {
		registered = m.PrimaryRegistered
	}
	// 先恢复已登记完整副本的真实ACK；manual只禁止恢复，不禁止只读校验。
	if (!m.manualBlocked() || (m.Ready && registered)) && (d.target == nil || d.target.current == nil) {
		target, err := d.openMember(ctx, opposite(m.Active))
		targetErr = err
		if err == nil {
			if d.target != nil {
				d.target.Close()
			}
			d.target = target
		}
	}
	// 人工锁只限制恢复动作，不停止健康 active 的服务或有界日志回收。
	if err := d.prune(ctx, time.Now()); err != nil {
		return errors.Join(targetErr, err)
	}
	if m.manualBlocked() {
		return nil
	}
	if targetErr != nil {
		return targetErr
	}
	if err := d.synchronize(ctx); err != nil {
		d.stabilityObserved = false
		if d.member().StableSince != nil {
			if persistErr := d.update(ctx, func(m *memberRecord) { m.StableSince = nil }); persistErr != nil {
				return persistErr
			}
		}
		if errors.Is(err, errReplicaJournalPressure) {
			maintenance := &replicaDomain{source: d.target}
			if gcErr := maintenance.prune(ctx, time.Now()); gcErr != nil {
				return gcErr
			}
		}
		return err
	}
	if d.member().Active == "standby" {
		return d.tryFailback(ctx)
	}
	return nil
}

func (d *replicaDomain) tryPromotion(ctx context.Context) error {
	m := d.member()
	if m.manualBlocked() || m.Freeze == nil {
		return nil
	}
	now, err := d.control.now()
	if err != nil {
		return err
	}
	if now.Boot != m.Freeze.Boot || now.Nanos < m.Freeze.Nanos {
		return d.update(ctx, func(m *memberRecord) {
			m.Manual = true
			m.State = "manual_required"
			m.Reason = "clock_proof_unavailable"
		})
	}
	if now.Nanos-m.Freeze.Nanos < int64(d.control.failoverConfig().FailureTimeout) {
		return nil
	}
	if d.target == nil || d.target.current == nil {
		target, err := d.openMember(ctx, opposite(m.Active))
		if err != nil {
			return err
		}
		d.target = target
	}
	p, err := d.promotionCheckpoint(ctx, m)
	if err != nil {
		var proof *promotionError
		if errors.As(err, &proof) && d.member().Reason != proof.phase {
			if persistErr := d.update(ctx, func(m *memberRecord) { m.Reason = proof.phase; m.State = "unavailable" }); persistErr != nil {
				return persistErr
			}
		}
		return err
	}
	bound := p.lossNanos
	if err = d.update(ctx, func(m *memberRecord) { m.LossNanos = &bound }); err != nil {
		return err
	}
	return d.switchTo(ctx, opposite(m.Active), p.position)
}

func (d *replicaDomain) synchronize(ctx context.Context) error {
	m := d.member()
	if d.target.applyPool == nil && d.target.current.dialect == MySQL {
		endpoint, _, _ := d.endpoint(opposite(m.Active))
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := openApplyPool(probe, d.target, endpoint)
		cancel()
		if err != nil {
			return err
		}
	}
	if !m.Ready {
		if m.Rebuild == nil && (m.State == "standby_active" || m.State == "resyncing" || m.State == "healthy") {
			inactive := m.Standby
			if m.Active == "standby" {
				inactive = m.Primary
			}
			if err := validateExistingReplica(ctx, d.target, m.Resource, inactive, replicaProof{}); err != nil {
				return err
			}
			err := d.alignEpoch(ctx)
			if err == nil {
				if err = d.catchUp(ctx); err != nil {
					return err
				}
				probe, cancel := context.WithTimeout(ctx, 2*time.Second)
				err = d.target.offlineGuard(probe, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(probe, "UPDATE replica_state SET ready=1 WHERE id=1")
					return err
				})
				cancel()
				if err != nil {
					return err
				}
				return d.update(ctx, func(m *memberRecord) { m.Ready = true; m.Checkpoint = nil })
			}
			if !errors.Is(err, ErrReplicaDiverged) {
				return err
			}
		}
		if err := d.seed(ctx); err != nil {
			return err
		}
	} else {
		err := d.catchUp(ctx)
		if errors.Is(err, ErrReplicaDiverged) {
			source, sourceErr := position(ctx, d.source.current.reader)
			target, targetErr := position(ctx, d.target.current.reader)
			if sourceErr != nil {
				return sourceErr
			}
			if targetErr != nil {
				return targetErr
			}
			if source.Epoch != target.Epoch {
				if err = d.alignEpoch(ctx); errors.Is(err, ErrReplicaDiverged) {
					err = d.seed(ctx)
				} else if err == nil {
					err = d.catchUp(ctx)
				}
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// 只有精确处于新 epoch 的祖先分叉点时才可增量；独有尾部必须全量舍弃。
func (d *replicaDomain) alignEpoch(ctx context.Context) error {
	source, err := position(ctx, d.source.current.reader)
	if err != nil {
		return err
	}
	target, err := position(ctx, d.target.current.reader)
	if err != nil {
		return err
	}
	if source.Epoch == target.Epoch {
		return ErrReplicaDiverged
	}
	type ancestor struct {
		id, parent, tx string
		seq            int64
	}
	var chain []ancestor
	next := source.Epoch
	for len(chain) < 1024 {
		var a ancestor
		a.id = next
		if err = d.source.current.reader.QueryRowContext(ctx, "SELECT parent_epoch_uuid,parent_seq,parent_tx_uuid FROM replica_epochs WHERE epoch_uuid=?", next).Scan(&a.parent, &a.seq, &a.tx); err != nil {
			return ErrReplicaDiverged
		}
		chain = append(chain, a)
		if a.parent == target.Epoch {
			if a.seq != target.Seq || a.tx != target.ID {
				return ErrReplicaDiverged
			}
			break
		}
		if a.parent == "" || a.parent == next {
			return ErrReplicaDiverged
		}
		next = a.parent
	}
	if len(chain) == 1024 {
		return ErrReplicaDiverged
	}
	return d.target.offlineGuard(ctx, func(tx *sql.Tx) error {
		p, err := position(ctx, tx)
		if err != nil {
			return err
		}
		if p != target {
			return ErrReplicaDiverged
		}
		for i := len(chain) - 1; i >= 0; i-- {
			a := chain[i]
			var count int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM replica_epochs WHERE epoch_uuid=?", a.id).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				if _, err = tx.ExecContext(ctx, "INSERT INTO replica_epochs VALUES(?,?,?,?)", a.id, a.parent, a.seq, a.tx); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid=?,head_seq=0,head_tx_uuid='',retained_from_seq=1,capture_mode='off',ready=1 WHERE id=1", source.Epoch)
		return err
	})
}

func (d *replicaDomain) tryFailback(ctx context.Context) error {
	m := d.member()
	if m.manualBlocked() || !m.Ready {
		return nil
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := d.target.offlineGuard(probe, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(probe, "UPDATE replica_state SET ready=ready WHERE id=1")
		return err
	})
	cancel()
	if err != nil {
		d.stabilityObserved = false
		if m.StableSince != nil {
			if persistErr := d.update(ctx, func(m *memberRecord) { m.StableSince = nil }); persistErr != nil {
				return persistErr
			}
		}
		return err
	}
	now, err := d.control.now()
	if err != nil {
		return err
	}
	if !d.stabilityObserved || m.StableSince == nil || m.StableSince.Boot != now.Boot || now.Nanos < m.StableSince.Nanos {
		if err = d.update(ctx, func(m *memberRecord) { m.StableSince = &now }); err != nil {
			return err
		}
		d.stabilityObserved = true
		return nil
	}
	if now.Nanos-m.StableSince.Nanos < int64(d.control.failoverConfig().RecoveryStable) {
		return nil
	}
	if err = d.drainForSwitch(ctx); err != nil {
		return d.drainFailure(ctx, err)
	}
	// 源准入已关闭且旧租约排空，最后一次追赶必须与源完全同 head。
	if err = d.catchUp(ctx); err != nil {
		return err
	}
	source, err := position(ctx, d.source.current.reader)
	if err != nil {
		return err
	}
	target, err := position(ctx, d.target.current.reader)
	if err != nil {
		return err
	}
	if source != target {
		return ErrReplicaDiverged
	}
	return d.switchTo(ctx, "primary", target)
}

// BootstrapRequired 只授权创建本次新集群的初始账号，不授权补造已登记成员的数据。
func (dbs *Databases) BootstrapRequired() bool {
	if dbs.replication == nil {
		return true
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.record.BootstrapPending && !r.record.Bootstrapped
}
func (dbs *Databases) CompleteBootstrap(ctx context.Context) error {
	if dbs.replication == nil {
		return nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	oldPending, oldComplete := r.record.BootstrapPending, r.record.Bootstrapped
	r.record.BootstrapPending = false
	r.record.Bootstrapped = true
	if err := r.persistLocked(ctx); err != nil {
		r.record.BootstrapPending = oldPending
		r.record.Bootstrapped = oldComplete
		return err
	}
	return nil
}
