package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
)

// ReplicationClusterID 只读本地控制记录，不连接端点、不创建文件。
func ReplicationClusterID(ctx context.Context, dataDir string) (string, error) {
	db, err := OpenReader(ctx, filepath.Join(dataDir, "storage-control.db"))
	if err != nil {
		return "", err
	}
	defer db.Close()
	var raw []byte
	if err = db.QueryRowContext(ctx, "SELECT record FROM storage_control WHERE id=1").Scan(&raw); err != nil {
		return "", ErrIdentityMismatch
	}
	var r controlRecord
	if err = json.Unmarshal(raw, &r); err != nil || r.Cluster == "" {
		return "", ErrIdentityMismatch
	}
	return r.Cluster, nil
}

func DisableReplication(ctx context.Context, cfg config.DatabaseConfig, dataDir, confirmation string) error {
	if cfg.Standby == nil {
		return ErrIdentityMismatch
	}
	cluster, err := ReplicationClusterID(ctx, dataDir)
	if err != nil || cluster != confirmation {
		return ErrIdentityMismatch
	}
	dbs, err := OpenOfflineDatabases(ctx, cfg, dataDir)
	if err != nil {
		return err
	}
	defer dbs.Close()
	r := dbs.replication
	if r == nil {
		return ErrIdentityMismatch
	}
	r.mu.Lock()
	if r.record.Cluster != confirmation {
		r.mu.Unlock()
		return ErrIdentityMismatch
	}
	if r.record.Pending != "" && r.record.Pending != "disable" {
		r.mu.Unlock()
		return ErrRestorePending
	}
	for _, m := range r.record.Domains {
		if m.Active != "primary" {
			r.mu.Unlock()
			return ErrUnavailable
		}
	}
	instant, clockErr := r.now()
	if clockErr != nil {
		r.mu.Unlock()
		return clockErr
	}
	for domain := range r.record.Domains {
		if recoveryPending(&r.record, domain, instant.Wall) != nil {
			r.mu.Unlock()
			return ErrRecoveryBusy
		}
	}
	r.mu.Unlock()
	// 所有网络前提先核实；control 事务不跨网络或写 gate 等待。
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		db := domainFrom(dbs, domain)
		if err = db.offlineGuard(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE storage_write_guard SET last_tx_uuid=last_tx_uuid WHERE id=1")
			return err
		}); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.record.Pending = "disable"
	for _, m := range r.record.Domains {
		m.Ready = false
		m.State = "disabling"
	}
	r.record.Revision++
	err = r.persistLocked(ctx)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err = r.invalidateInactive(ctx, []Domain{MainDomain, HistoryDomain}); err != nil {
		return err
	}
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		db := domainFrom(dbs, domain)
		if err = db.offlineGuard(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE replica_state SET capture_mode='off',ready=0 WHERE id=1")
			return err
		}); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Disabled = true
	r.record.Pending = ""
	r.record.Revision++
	return r.persistLocked(ctx)
}

// resetRestoreEpoch 与逻辑行及 restore marker 共用目标 SQL 事务，不能重放旧分支。
func resetRestoreEpoch(ctx context.Context, tx *Tx) error {
	if tx.lease.db.replica == nil {
		return nil
	}
	if !tx.lease.db.restoreAllowed {
		return ErrRestorePending
	}
	p, err := position(ctx, tx.tx)
	if err != nil {
		return err
	}
	epoch := uuid.NewString()
	if _, err = tx.tx.ExecContext(ctx, "DELETE FROM replica_changes"); err != nil {
		return safeFailure(err)
	}
	if _, err = tx.tx.ExecContext(ctx, "DELETE FROM replica_transactions"); err != nil {
		return safeFailure(err)
	}
	if _, err = tx.tx.ExecContext(ctx, "INSERT INTO replica_epochs VALUES(?,?,?,?)", epoch, p.Epoch, p.Seq, p.ID); err != nil {
		return safeFailure(err)
	}
	_, err = tx.tx.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid=?,head_seq=0,head_tx_uuid='',retained_from_seq=1,open_tx_uuid='',next_ordinal=0,owner_connection=0,journal_bytes=0,ready=0,capture_mode='off' WHERE id=1", epoch)
	return safeFailure(err)
}

type restoreContextKey struct{}

func (db *Database) setRestoring(value bool) { db.mu.Lock(); db.restoreAllowed = value; db.mu.Unlock() }
func (db *Database) restoreAccess(ctx context.Context) (bool, bool) {
	db.mu.Lock()
	pending := db.restoreAllowed
	db.mu.Unlock()
	return pending, pending && ctx.Value(restoreContextKey{}) == db
}
