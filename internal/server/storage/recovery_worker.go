package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (m memberRecord) recoveryAuthorized() bool {
	return m.Incident != "" && m.ResumeIncident == m.Incident
}
func (m memberRecord) manualBlocked() bool { return m.Manual && !m.recoveryAuthorized() }
func (d *replicaDomain) coordinatorStep(ctx context.Context) error {
	if d.control == nil {
		return d.coordinatorAutomatic(ctx)
	}
	handled, err := d.runRecoveryOperation(ctx)
	if handled {
		if observeErr := d.observeRecovery(ctx, err, false); observeErr != nil {
			return observeErr
		}
		return err
	}
	err = d.coordinatorAutomatic(ctx)
	if observeErr := d.observeRecovery(ctx, err, true); observeErr != nil {
		return observeErr
	}
	return err
}
func (d *replicaDomain) observeRecovery(ctx context.Context, stepErr error, synchronized bool) error {
	r := d.control
	serving := d.source.Ready()
	if stepErr != nil && !storageFailure(stepErr) && !errors.Is(stepErr, context.DeadlineExceeded) && !errors.Is(stepErr, ErrUnavailable) {
		return nil
	}
	targetObserved, targetReachableNow := false, false
	member := d.member()
	if member.manualBlocked() && member.Ready {
		if d.target == nil && !serving {
			target, probeErr := d.openMember(ctx, opposite(member.Active))
			targetObserved = true
			targetReachableNow = probeErr == nil
			if probeErr == nil {
				d.target = target
			}
		} else if d.target != nil && d.target.current != nil {
			phase, cancel := context.WithTimeout(ctx, 2*time.Second)
			targetReachableNow = d.target.current.reader.PingContext(phase) == nil
			cancel()
			targetObserved = true
		} else {
			targetObserved = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[d.domain]
	old := *m
	targetReachable := m.StandbyReachable
	if m.Active == "standby" {
		targetReachable = m.PrimaryReachable
	}
	if targetObserved {
		targetReachable = targetReachableNow
	}
	if synchronized && !m.manualBlocked() && stepErr != nil && serving {
		targetReachable = false
	}
	if m.Active == "primary" {
		m.PrimaryReachable = serving
		m.StandbyReachable = targetReachable
	} else {
		m.StandbyReachable = serving
		m.PrimaryReachable = targetReachable
	}
	status := "caught_up"
	if !targetReachable {
		status = "offline"
	} else if !m.Ready {
		status = "initializing"
	}
	if m.State == "rebuild_required" {
		status = "rebuild_required"
	}
	if m.ReplicationStatus == "error" && !m.Ready {
		status = "error"
	}
	m.ReplicationStatus = status
	if !targetReachable && serving && (old.Ready || old.LastSyncedAt != nil) && m.FailureSince.IsZero() {
		now, err := r.now()
		if err != nil {
			return err
		}
		m.FailureSince = now.Wall
	}
	if targetReachable && serving && m.Ready && !m.Manual && m.Active == "primary" {
		m.FailureSince = time.Time{}
	}
	if old.PrimaryReachable == m.PrimaryReachable && old.StandbyReachable == m.StandbyReachable && old.ReplicationStatus == m.ReplicationStatus && old.FailureSince.Equal(m.FailureSince) {
		return nil
	}
	if err := r.persistLocked(ctx); err != nil {
		*m = old
		return err
	}
	return nil
}
func recoveryCode(err error) string {
	switch {
	case errors.Is(err, ErrConfirmationStale):
		return "storage_confirmation_stale"
	case errors.Is(err, ErrIdentityMismatch), errors.Is(err, ErrSchemaIncompatible), errors.Is(err, ErrSchemaTooNew), errors.Is(err, ErrReplicaDiverged):
		return "storage_identity_mismatch"
	case errors.Is(err, ErrCommitUnknown):
		return "storage_commit_unknown"
	case errors.Is(err, ErrManualRequired):
		return "storage_manual_required"
	case errors.Is(err, ErrRecoveryBusy), IsBusy(err):
		return "storage_busy"
	case errors.Is(err, ErrTransactionTooLarge):
		return "storage_transaction_too_large"
	default:
		return "storage_unavailable"
	}
}
func (d *replicaDomain) operationProgress(ctx context.Context, phase string, rows int64) error {
	r := d.control
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, op := range r.record.Operations {
		if op.Domain == d.domain && op.Status == "running" {
			old := *op
			op.Phase = &phase
			op.RowsCopied = rows
			if err := r.persistLocked(ctx); err != nil {
				*op = old
				return err
			}
			return nil
		}
	}
	return nil
}
func (d *replicaDomain) acknowledgeRecovery(ctx context.Context, p replicaPosition) error {
	if d.control == nil {
		return nil
	}
	r := d.control
	now, err := r.now()
	if err != nil {
		return err
	}
	var barrier *freezePoint
	if p.Seq > 0 {
		var b freezePoint
		err = d.target.current.reader.QueryRowContext(ctx, "SELECT boot_id,barrier_ns FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=?", p.Epoch, p.Seq, p.ID).Scan(&b.Boot, &b.Nanos)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return safeFailure(err)
		}
		if err == nil {
			barrier = &b
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[d.domain]
	targetReachable := m.StandbyReachable
	if m.Active == "standby" {
		targetReachable = m.PrimaryReachable
	}
	if m.LastAcknowledged != nil && *m.LastAcknowledged == p && targetReachable {
		return nil
	}
	old := *m
	stamp := now.Wall
	m.LastSyncedAt = &stamp
	m.LastAcknowledged = &p
	m.LastBarrier = barrier
	if m.Active == "primary" {
		m.StandbyReachable = true
	} else {
		m.PrimaryReachable = true
	}
	if err = r.persistLocked(ctx); err != nil {
		*m = old
		return err
	}
	d.lastSynced.Store(stamp.UnixNano())
	return nil
}
func (d *replicaDomain) runRecoveryOperation(ctx context.Context) (bool, error) {
	r := d.control
	var err error
	if err = d.checkManual(ctx); err != nil {
		return true, err
	}
	serving := d.source.Ready()
	r.mu.Lock()
	var op *recoveryOperationRecord
	for _, candidate := range r.record.Operations {
		if candidate.Domain == d.domain && (candidate.Status == "queued" || candidate.Status == "running") {
			v := *candidate
			op = &v
			break
		}
	}
	if op == nil {
		r.mu.Unlock()
		return false, nil
	}
	m := r.record.Domains[d.domain]
	if op.Status == "queued" {
		if op.Epoch != m.Epoch || op.Incident != m.Incident || op.SourceRole != m.Active || op.SourceInstance != roleInstance(m, m.Active) || op.TargetInstance != roleInstance(m, op.Target) || !recoveryAllowed(m, op.Action, op.Target, serving) {
			r.mu.Unlock()
			return true, d.finishRecoveryOperation(ctx, op.ID, ErrConfirmationStale)
		}
		old := *r.record.Operations[op.ID]
		oldMember := *m
		phase := "validating"
		r.record.Operations[op.ID].Status = "running"
		r.record.Operations[op.ID].Phase = &phase
		if op.Action == "resume" || op.Action == "promote" {
			m.ResumeIncident = op.Incident
		}
		if err = r.persistLocked(ctx); err != nil {
			*r.record.Operations[op.ID] = old
			*m = oldMember
			r.mu.Unlock()
			return true, err
		}
	}
	r.mu.Unlock()
	current := d.member()
	if op.Action == "promote" && op.AppliedEpoch != "" && current.Active == op.Target && current.Epoch == op.AppliedEpoch && current.Intent == nil {
		return true, d.finishRecoveryOperation(ctx, op.ID, nil)
	}
	if current.Intent != nil {
		if current.Intent.Target != op.Target {
			return true, d.finishRecoveryOperation(ctx, op.ID, ErrConfirmationStale)
		}
		if err = d.finishSwitch(ctx); err != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, err)
		}
		if op.Action == "promote" {
			return true, d.finishRecoveryOperation(ctx, op.ID, nil)
		}
	}
	if op.Action == "resume" {
		current = d.member()
		if op.Incident == "" || current.Incident != op.Incident || current.ResumeIncident != op.Incident {
			return true, d.finishRecoveryOperation(ctx, op.ID, ErrConfirmationStale)
		}
		if op.Phase != nil && *op.Phase == "validating" {
			if err = d.operationProgress(ctx, "catching_up", op.RowsCopied); err != nil {
				return true, err
			}
		}
		if err = d.coordinatorAutomatic(ctx); err != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, err)
		}
		current = d.member()
		if current.Active == "primary" && current.Ready && current.Freeze == nil && d.source.Ready() {
			return true, d.finishRecoveryOperation(ctx, op.ID, nil)
		}
		return true, nil
	}
	if op.Action == "rebuild" {
		current = d.member()
		if current.Active == op.Target || current.Epoch != op.Epoch || !d.source.Ready() {
			return true, d.finishRecoveryOperation(ctx, op.ID, ErrConfirmationStale)
		}
		if err = d.ensureRecoveryTarget(ctx, op.Target, true); err != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, err)
		}
		if err = d.seed(ctx); err != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, err)
		}
		return true, d.finishRecoveryOperation(ctx, op.ID, nil)
	}
	if op.Action == "promote" {
		current = d.member()
		if !op.AcceptDataLoss || d.source.Ready() || current.Active == op.Target || !current.Ready || current.Rebuild != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, ErrManualRequired)
		}
		if err = d.ensureRecoveryTarget(ctx, op.Target, false); err != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, err)
		}
		if err = d.operationProgress(ctx, "switching", 0); err != nil {
			return true, err
		}
		var p replicaPosition
		var lossBound *int64
		observed, clockErr := r.now()
		if clockErr != nil {
			return true, clockErr
		}
		phase, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = d.target.offlineGuard(phase, func(tx *sql.Tx) error {
			var ready int
			if err := tx.QueryRowContext(phase, "SELECT ready FROM replica_state WHERE id=1").Scan(&ready); err != nil {
				return err
			}
			if ready != 1 {
				return ErrManualRequired
			}
			var err error
			p, err = position(phase, tx)
			if err != nil {
				return err
			}
			if p.Epoch != current.Epoch {
				return ErrIdentityMismatch
			}
			if current.Freeze != nil && observed.Boot == current.Freeze.Boot {
				var boot string
				var barrier int64
				err = tx.QueryRowContext(phase, "SELECT boot_id,barrier_ns FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=?", p.Epoch, p.Seq, p.ID).Scan(&boot, &barrier)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil && boot == current.Freeze.Boot && barrier >= 0 && barrier <= current.Freeze.Nanos {
					bound := current.Freeze.Nanos - barrier
					lossBound = &bound
				}
			}
			return nil
		})
		cancel()
		if err != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, err)
		}
		if err = d.update(ctx, func(m *memberRecord) { m.LossNanos = lossBound }); err != nil {
			return true, err
		}
		if err = d.switchTo(ctx, op.Target, p); err != nil {
			return true, d.finishRecoveryOperation(ctx, op.ID, err)
		}
		return true, d.finishRecoveryOperation(ctx, op.ID, nil)
	}
	return true, d.finishRecoveryOperation(ctx, op.ID, ErrManualRequired)
}
func (d *replicaDomain) ensureRecoveryTarget(ctx context.Context, role string, rebuild bool) error {
	m := d.member()
	if role == m.Active {
		return ErrManualRequired
	}
	endpoint, id, registered := d.endpoint(role)
	if !registered {
		return ErrIdentityMismatch
	}
	if d.target == nil || d.target.current == nil {
		var target *Database
		var err error
		if rebuild {
			resource, instance, inspectErr := inspectMember(ctx, endpoint, d.domain, d.control.dataDir)
			if inspectErr != nil {
				return inspectErr
			}
			if resource != m.Resource || instance != id {
				return ErrIdentityMismatch
			}
			target, err = OpenDomain(ctx, endpoint, d.domain, d.control.dataDir)
		} else {
			target, err = d.openMember(ctx, role)
		}
		if err != nil {
			return err
		}
		if d.target != nil {
			d.target.Close()
		}
		d.target = target
	}
	proof := replicaProof{Checkpoint: m.checkpointFor(id), Ready: m.Ready && !rebuild, Rebuilding: rebuild || (!m.Ready && m.Rebuild != nil)}
	if err := validateExistingReplica(ctx, d.target, m.Resource, id, proof); err != nil {
		return err
	}
	if d.target.current.dialect == MySQL && d.target.applyPool == nil {
		return openApplyPool(ctx, d.target, endpoint)
	}
	return nil
}
func (d *replicaDomain) finishRecoveryOperation(ctx context.Context, id string, cause error) error {
	r := d.control
	now, err := r.now()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op := r.record.Operations[id]
	if op == nil {
		return ErrRecoveryNotFound
	}
	if op.Status == "succeeded" || op.Status == "failed" {
		return nil
	}
	if cause != nil && (ctx.Err() != nil || (r.record.Domains[d.domain].Intent != nil && r.record.Domains[d.domain].Intent.Target == op.Target)) {
		return nil
	}
	oldOp := *op
	m := r.record.Domains[d.domain]
	oldMember := *m
	op.FinishedAt = now.Wall
	if cause == nil {
		phase := "done"
		op.Status = "succeeded"
		op.Phase = &phase
		if op.Action == "resume" && op.Incident != "" && m.Incident == op.Incident {
			m.Manual = false
			m.ResumeIncident = ""
			m.Reason = ""
			if m.Active == "primary" {
				m.State = "healthy"
				m.FailureSince = time.Time{}
			}
		}
		if op.Action == "rebuild" && m.Reason == "replica_integrity_error" {
			m.Reason = ""
		}
	} else {
		op.Status = "failed"
		op.ErrorCode = recoveryCode(cause)
		if !errors.Is(cause, ErrConfirmationStale) {
			m.Manual = true
			m.State = "manual_required"
			m.Reason = "recovery_failed"
			if errors.Is(cause, ErrIdentityMismatch) || errors.Is(cause, ErrSchemaIncompatible) || errors.Is(cause, ErrReplicaDiverged) {
				m.Ready = false
				m.ReplicationStatus = "error"
				m.Reason = "replica_integrity_error"
			}
		}
		m.ResumeIncident = ""
	}
	if op.Action == "promote" {
		m.ResumeIncident = ""
	}
	if err = r.persistLocked(ctx); err != nil {
		*op = oldOp
		*m = oldMember
		return err
	}
	return nil
}
