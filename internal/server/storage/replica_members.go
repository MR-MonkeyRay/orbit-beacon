package storage

import (
	"context"
	"database/sql"
	"errors"
	"orbitbeacon/internal/config"
)

func validateEndpointSeparation(ctx context.Context, cfg config.DatabaseConfig) error {
	seen := map[string]bool{}
	for _, endpoint := range []config.DatabaseEndpoint{cfg.Primary, *cfg.Standby} {
		if endpoint.Driver != "mysql" {
			continue
		}
		for _, domain := range []Domain{MainDomain, HistoryDomain} {
			c, err := mysqlConfig(endpoint, domain)
			if err != nil {
				return err
			}
			pool, err := mysqlConnectorDB(c)
			if err != nil {
				return err
			}
			var server, database string
			err = pool.QueryRowContext(ctx, "SELECT @@server_uuid,DATABASE()").Scan(&server, &database)
			pool.Close()
			if err != nil {
				return safeFailure(err)
			}
			key := server + "/" + database
			if seen[key] {
				return ErrIdentityMismatch
			}
			seen[key] = true
		}
	}
	return nil
}

// inactive 的资格先由 control 撤销；可达时同步清 SQL ready，离线副本不能阻止当前库恢复。
func (r *replication) invalidateInactive(ctx context.Context, domains []Domain) error {
	for _, domain := range domains {
		r.mu.Lock()
		m := *r.record.Domains[domain]
		cfg, dataDir := r.cfg, r.dataDir
		r.mu.Unlock()
		endpoint := *cfg.Standby
		id := m.Standby
		if m.Active == "standby" {
			endpoint = cfg.Primary
			id = m.Primary
		}
		registered := m.StandbyRegistered
		if m.Active == "standby" {
			registered = m.PrimaryRegistered
		}
		if !registered {
			continue
		}
		resource, instance, probeErr := inspectMember(ctx, endpoint, domain, dataDir)
		if probeErr != nil {
			if IsConnectionFailure(probeErr) {
				continue
			}
			return probeErr
		}
		if resource != m.Resource || instance != id {
			return ErrIdentityMismatch
		}
		db := r.domains[domain].target
		owned := false
		if db == nil {
			var err error
			db, err = OpenDomain(ctx, endpoint, domain, dataDir)
			if err != nil {
				if IsConnectionFailure(err) {
					continue
				}
				return err
			}
			owned = true
		}
		resource, instance, err := replicaIdentity(ctx, db)
		if err != nil || resource != m.Resource || instance != id {
			if owned {
				db.Close()
			}
			return ErrIdentityMismatch
		}
		_, err = db.current.writer.ExecContext(ctx, "UPDATE replica_state SET ready=0 WHERE id=1")
		if owned {
			db.Close()
		}
		if err != nil && !IsConnectionFailure(err) {
			return safeFailure(err)
		}
	}
	return nil
}

func (db *Database) offlineGuard(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case db.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-db.writeGate }()
	if err := db.resolvePending(ctx); err != nil {
		return err
	}
	opts := &sql.TxOptions{}
	if db.current.dialect == MySQL {
		opts.Isolation = sql.LevelReadCommitted
	}
	tx, err := db.current.writer.BeginTx(ctx, opts)
	if err != nil {
		return safeFailure(err)
	}
	defer tx.Rollback()
	q := "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1"
	if db.current.dialect == MySQL {
		q += " FOR UPDATE"
	}
	var id string
	if err = tx.QueryRowContext(ctx, q).Scan(&id); err != nil {
		return safeFailure(err)
	}
	if err = fn(tx); err != nil {
		return safeFailure(err)
	}
	if err = tx.Commit(); err != nil {
		return errors.Join(ErrCommitUnknown, safeFailure(err))
	}
	return nil
}
