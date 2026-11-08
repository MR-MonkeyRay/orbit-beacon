package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/securefs"
)

type memberRecord struct {
	Resource, Primary, Standby, Epoch, Active, State string
	Ready, PrimaryRegistered, StandbyRegistered      bool
	Checkpoint                                       *replicaCheckpoint
	Generation                                       uint64
	Freeze                                           *freezePoint
	FailureSince                                     time.Time
	StableSince                                      *freezePoint
	Manual                                           bool
	Reason                                           string
	Intent                                           *switchIntent
	LossNanos                                        *int64
	Rebuild                                          *rebuildOperation
}
type controlRecord struct {
	Cluster, Binding, Pending, Finished string
	Disabled                            bool
	Revision                            uint64
	Domains                             map[Domain]*memberRecord
	RestoreDomains                      []Domain
	Bootstrapped                        bool
	BootstrapPending                    bool
}
type replication struct {
	mu      sync.Mutex
	control *sql.DB
	record  controlRecord
	domains map[Domain]*replicaDomain
	cancel  context.CancelFunc
	done    chan struct{}
	cfg     config.DatabaseConfig
	dataDir string
	clock   func() (string, int64, time.Time, error)
}

func configBinding(cfg config.DatabaseConfig) string {
	type endpoint struct {
		Driver, Network, Address, Main, History string
		TLS                                     *config.DatabaseTLS
	}
	bind := func(e config.DatabaseEndpoint) endpoint {
		d := e.Driver
		if d == "" {
			d = "sqlite"
		}
		network := e.Network
		if d == "mysql" && network == "" {
			network = "tcp"
		}
		main, history := e.MainDatabase, e.HistoryDatabase
		if d == "mysql" {
			if main == "" {
				main = "orbitbeacon"
			}
			if history == "" {
				history = "orbitbeacon_history"
			}
		}
		return endpoint{d, network, e.Address, main, history, e.TLS}
	}
	values := []endpoint{bind(cfg.Primary)}
	if cfg.Standby != nil {
		values = append(values, bind(*cfg.Standby))
	}
	b, _ := json.Marshal(values)
	return string(b)
}
func openControl(ctx context.Context, dataDir string, create bool) (*replication, error) {
	path := filepath.Join(dataDir, "storage-control.db")
	_, exists, err := validateDatabasePath(path)
	if err != nil {
		return nil, err
	}
	if !exists && !create {
		return nil, ErrIdentityMismatch
	}
	db, err := openDatabase(ctx, path)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*replication, error) { db.Close(); return nil, e }
	if !exists {
		if _, err = db.ExecContext(ctx, "CREATE TABLE storage_control(id INTEGER PRIMARY KEY CHECK(id=1),record TEXT NOT NULL)"); err != nil {
			return fail(err)
		}
		dir, err := os.Open(dataDir)
		if err != nil {
			return fail(err)
		}
		err = errors.Join(dir.Sync(), dir.Close())
		if err != nil {
			return fail(err)
		}
	}
	r := &replication{control: db, domains: map[Domain]*replicaDomain{}, dataDir: dataDir}
	var raw []byte
	err = db.QueryRowContext(ctx, "SELECT record FROM storage_control WHERE id=1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) && !exists {
		return r, nil
	}
	if err != nil {
		return fail(ErrIdentityMismatch)
	}
	if err = json.Unmarshal(raw, &r.record); err != nil || r.record.Cluster == "" || r.record.Domains == nil {
		return fail(ErrIdentityMismatch)
	}
	return r, nil
}
func (r *replication) persistLocked(ctx context.Context) error {
	raw, err := json.Marshal(r.record)
	if err != nil {
		return err
	}
	_, err = r.control.ExecContext(ctx, "INSERT INTO storage_control(id,record) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET record=excluded.record", string(raw))
	return safeFailure(err)
}
func (r *replication) revoke(ctx context.Context, domain Domain, state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[domain]
	if m == nil {
		return ErrIdentityMismatch
	}
	if !m.Ready && (m.State == state || (m.State == "error" && state == "rebuild_required")) {
		return nil
	}
	old := *m
	m.Ready = false
	m.State = state
	r.record.Revision++
	if err := r.persistLocked(ctx); err != nil {
		*m = old
		r.record.Revision--
		return err
	}
	return nil
}
func domainFrom(dbs *Databases, domain Domain) *Database {
	if domain == HistoryDomain {
		return dbs.History
	}
	return dbs.Main
}
func setDomain(dbs *Databases, domain Domain, db *Database) {
	if domain == HistoryDomain {
		dbs.History = db
	} else {
		dbs.Main = db
	}
}

func openConfigured(ctx context.Context, cfg config.DatabaseConfig, dataDir string, offline bool, only Domain) (*Databases, error) {
	domains := []Domain{MainDomain, HistoryDomain}
	if only != "" {
		if only != MainDomain && only != HistoryDomain {
			return nil, ErrUnavailable
		}
		domains = []Domain{only}
	}
	result := &Databases{}
	fail := func(err error) (*Databases, error) { result.Close(); return nil, err }
	controlExists, err := securefs.ValidateFile(filepath.Join(dataDir, "storage-control.db"))
	if err != nil {
		return nil, err
	}
	if only != "" && cfg.Standby != nil && !controlExists {
		return nil, ErrIdentityMismatch
	}
	if cfg.Standby == nil {
		if controlExists {
			r, err := openControl(ctx, dataDir, false)
			if err != nil {
				return nil, err
			}
			disabled := r.record.Disabled
			var endpoints []json.RawMessage
			decodeErr := json.Unmarshal([]byte(r.record.Binding), &endpoints)
			r.control.Close()
			if !disabled || decodeErr != nil || len(endpoints) != 2 || "["+string(endpoints[0])+"]" != configBinding(cfg) {
				return nil, ErrIdentityMismatch
			}
		}
		for _, domain := range domains {
			db, err := OpenDomain(ctx, cfg.Primary, domain, dataDir)
			if err != nil {
				if domain == HistoryDomain && cfg.Primary.Driver != "mysql" {
					return result, err
				}
				return fail(err)
			}
			setDomain(result, domain, db)
			registered, err := hasReplica(ctx, db)
			if err != nil {
				return fail(err)
			}
			if registered {
				var mode string
				if err = db.current.reader.QueryRowContext(ctx, "SELECT capture_mode FROM replica_state WHERE id=1").Scan(&mode); err != nil || mode != "off" {
					return fail(ErrIdentityMismatch)
				}
				if !controlExists {
					return fail(ErrIdentityMismatch)
				}
			}
		}
		return result, nil
	}
	if cfg.Primary.Driver != "mysql" {
		return nil, ErrUnsupportedTopology
	}
	if !controlExists {
		if err = validateEndpointSeparation(ctx, cfg); err != nil {
			return fail(err)
		}
	}
	// 控制丢失时先检查已有主库身份，不能重新猜测集群。
	if !controlExists {
		for _, domain := range []Domain{MainDomain, HistoryDomain} {
			db, err := OpenDomain(ctx, cfg.Primary, domain, dataDir)
			if err != nil {
				return fail(err)
			}
			registered, err := hasReplica(ctx, db)
			db.Close()
			if err != nil {
				return fail(err)
			}
			if registered {
				return fail(ErrIdentityMismatch)
			}
		}
	}
	r, err := openControl(ctx, dataDir, !controlExists)
	if err != nil {
		return fail(err)
	}
	result.replication = r
	r.cfg = cfg
	r.dataDir = dataDir
	if r.record.Cluster == "" {
		r.record = controlRecord{Cluster: uuid.NewString(), Binding: configBinding(cfg), Revision: 1, BootstrapPending: true, Domains: map[Domain]*memberRecord{}}
		for _, domain := range []Domain{MainDomain, HistoryDomain} {
			r.record.Domains[domain] = &memberRecord{Resource: uuid.NewString(), Primary: uuid.NewString(), Standby: uuid.NewString(), Epoch: uuid.NewString(), Active: "primary", State: "initializing", Generation: 1}
		}
		if err = r.persistLocked(ctx); err != nil {
			return fail(err)
		}
	}
	if r.record.Disabled || r.record.Binding != configBinding(cfg) {
		return fail(ErrIdentityMismatch)
	}
	if !offline && controlExists {
		if err = r.openRegistered(ctx, result, domains); err != nil {
			return fail(err)
		}
		return result, nil
	}
	if r.record.Pending != "" && (!offline || only != "") {
		return fail(ErrRestorePending)
	}
	for _, domain := range domains {
		m := r.record.Domains[domain]
		if m == nil {
			return fail(ErrIdentityMismatch)
		}
		if m.Intent != nil || (m.Freeze != nil && r.record.Pending == "") {
			return fail(ErrUnavailable)
		}
		endpoint := cfg.Primary
		activeID := m.Primary
		if m.Active == "standby" {
			endpoint = *cfg.Standby
			activeID = m.Standby
		} else if m.Active != "primary" {
			return fail(ErrIdentityMismatch)
		}
		expectedRegistered := m.PrimaryRegistered
		if m.Active == "standby" {
			expectedRegistered = m.StandbyRegistered
		}
		if expectedRegistered {
			resource, instance, identityErr := inspectMember(ctx, endpoint, domain, dataDir)
			if identityErr != nil {
				return fail(identityErr)
			}
			if resource != m.Resource || instance != activeID {
				return fail(ErrIdentityMismatch)
			}
		}
		source, err := OpenDomain(ctx, endpoint, domain, dataDir)
		if err != nil {
			return fail(err)
		}
		setDomain(result, domain, source)
		activeProof := replicaProof{Checkpoint: m.checkpointFor(activeID)}
		activeProof.Ready = activeProof.Checkpoint != nil && r.record.Pending == ""
		if err = installReplica(ctx, source, m.Resource, activeID, m.Epoch, activeProof); err != nil {
			return fail(err)
		}
		if !expectedRegistered {
			if m.Active == "standby" {
				m.StandbyRegistered = true
			} else {
				m.PrimaryRegistered = true
			}
			if err = r.persistLocked(ctx); err != nil {
				return fail(err)
			}
		}
		p, err := position(ctx, source.current.reader)
		if err != nil {
			return fail(err)
		}
		if r.record.Pending == "" && p.Epoch != m.Epoch {
			return fail(ErrIdentityMismatch)
		}
		rd := &replicaDomain{source: source, resource: m.Resource, epoch: p.Epoch, control: r, domain: domain}
		source.replica = rd
		r.domains[domain] = rd
		if r.record.Pending != "" && r.record.Pending != "disable" {
			for _, affected := range r.record.RestoreDomains {
				if affected == domain {
					source.setRestoring(true)
				}
			}
		}
		rd.revoke = func(ctx context.Context) error { return r.revoke(ctx, domain, "rebuild_required") }
		if only != "" && r.record.Pending == "" {
			if _, err = source.current.writer.ExecContext(ctx, "UPDATE replica_state SET capture_mode='source' WHERE id=1"); err != nil {
				return fail(safeFailure(err))
			}
		}
		if only != "" || offline {
			continue
		}
		inactive := *cfg.Standby
		inactiveID := m.Standby
		if m.Active == "standby" {
			inactive = cfg.Primary
			inactiveID = m.Primary
		}
		targetRegistered := m.StandbyRegistered
		if m.Active == "standby" {
			targetRegistered = m.PrimaryRegistered
		}
		if targetRegistered {
			resource, instance, identityErr := inspectMember(ctx, inactive, domain, dataDir)
			if identityErr != nil {
				return fail(identityErr)
			}
			if resource != m.Resource || instance != inactiveID {
				return fail(ErrIdentityMismatch)
			}
		}
		proof := replicaProof{Checkpoint: m.checkpointFor(inactiveID), Ready: m.Ready, Rebuilding: !m.Ready && (m.State == "rebuilding" || m.State == "rebuild_required")}
		target, err := openReplicaTarget(ctx, inactive, domain, dataDir, m.Resource, inactiveID, m.Epoch, proof)
		if err != nil {
			return fail(err)
		}
		rd.target = target
		targetPosition, positionErr := position(ctx, target.current.reader)
		if positionErr != nil {
			return fail(positionErr)
		}
		rd.applied.Store(targetPosition.Seq)
		if m.Active == "standby" {
			m.PrimaryRegistered = true
		} else {
			m.StandbyRegistered = true
		}
		if err = r.persistLocked(ctx); err != nil {
			return fail(err)
		}
		if err = openApplyPool(ctx, target, inactive); err != nil {
			return fail(err)
		}
		if _, err = source.current.writer.ExecContext(ctx, "UPDATE replica_state SET capture_mode='source' WHERE id=1"); err != nil {
			return fail(safeFailure(err))
		}
	}
	return result, nil
}
func OpenActiveDomain(ctx context.Context, cfg config.DatabaseConfig, domain Domain, dataDir string) (*Database, error) {
	dbs, err := openConfigured(ctx, cfg, dataDir, true, domain)
	if err != nil {
		return nil, err
	}
	db := domainFrom(dbs, domain)
	if dbs.replication != nil {
		db.ownedControl = dbs.replication.control
	}
	return db, nil
}
func OpenOfflineDatabases(ctx context.Context, cfg config.DatabaseConfig, dataDir string) (*Databases, error) {
	return openConfigured(ctx, cfg, dataDir, true, "")
}

func openReplicaTarget(ctx context.Context, e config.DatabaseEndpoint, domain Domain, dataDir, resource, instance, epoch string, proof replicaProof) (*Database, error) {
	// 未登记的目标必须是真正空 schema，不能把有业务表但零行的库当新库。
	if e.Driver == "mysql" {
		cfg, err := mysqlConfig(e, domain)
		if err != nil {
			return nil, err
		}
		conn, err := mysqlConnectorDB(cfg)
		if err != nil {
			return nil, err
		}
		var all, identity int
		err = conn.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(table_name='replica_identity'),0) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&all, &identity)
		conn.Close()
		if err != nil {
			return nil, safeFailure(err)
		}
		if all > 0 && identity == 0 {
			return nil, ErrIdentityMismatch
		}
	} else {
		path := filepath.Join(dataDir, "orbitbeacon.db")
		if domain == HistoryDomain {
			path = filepath.Join(dataDir, "metrics.db")
		}
		exists, err := securefs.ValidateFile(path)
		if err != nil {
			return nil, err
		}
		if exists {
			info, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if info.Size() > 0 {
				raw, err := OpenReader(ctx, path)
				if err != nil {
					return nil, err
				}
				var n int
				err = raw.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name='replica_identity' AND type='table'").Scan(&n)
				raw.Close()
				if err != nil || n != 1 {
					return nil, ErrIdentityMismatch
				}
			}
		}
	}
	target, err := OpenDomain(ctx, e, domain, dataDir)
	if err != nil {
		return nil, err
	}
	if err = installReplica(ctx, target, resource, instance, epoch, proof); err != nil {
		target.Close()
		return nil, err
	}
	return target, nil
}

func (dbs *Databases) CaptureRestoreMetadata(ctx context.Context) (json.RawMessage, error) {
	if dbs.replication == nil {
		return nil, nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(r.record)
}
func (dbs *Databases) BeginRestore(ctx context.Context, operationID string, domains []Domain) error {
	if dbs.replication == nil {
		return nil
	}
	if operationID == "" || len(operationID) > 64 {
		return ErrIdentityMismatch
	}
	if err := dbs.StopReplication(); err != nil {
		return err
	}
	r := dbs.replication
	r.mu.Lock()
	if r.record.Pending != "" && r.record.Pending != operationID {
		r.mu.Unlock()
		return ErrRestorePending
	}
	if r.record.Pending == "" {
		for _, domain := range domains {
			m := r.record.Domains[domain]
			if m == nil {
				r.mu.Unlock()
				return ErrIdentityMismatch
			}
			if m.Freeze != nil || m.Intent != nil {
				r.mu.Unlock()
				return ErrUnavailable
			}
		}
	}
	r.record.Pending = operationID
	r.record.RestoreDomains = append([]Domain(nil), domains...)
	r.record.Revision++
	for _, domain := range domains {
		m := r.record.Domains[domain]
		if m == nil {
			r.mu.Unlock()
			return ErrIdentityMismatch
		}
		m.Ready = false
		m.State = "restore_pending"
	}
	err := r.persistLocked(ctx)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	for _, domain := range domains {
		db := domainFrom(dbs, domain)
		if db == nil {
			return ErrUnavailable
		}
		db.setRestoring(true)
	}
	if err = r.invalidateInactive(ctx, domains); err != nil {
		return err
	}
	for _, domain := range domains {
		db := domainFrom(dbs, domain)
		if err = db.offlineGuard(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE replica_state SET capture_mode='off',ready=0 WHERE id=1")
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
func (dbs *Databases) FinishRestore(ctx context.Context, operationID string, old json.RawMessage, rollback bool) error {
	if dbs.replication == nil {
		return nil
	}
	r := dbs.replication
	r.mu.Lock()
	finished := operationID + "/commit"
	if rollback {
		finished = operationID + "/rollback"
	}
	if r.record.Pending == "" && r.record.Finished == finished {
		r.mu.Unlock()
		return nil
	}
	if r.record.Pending != operationID {
		r.mu.Unlock()
		return ErrRestorePending
	}
	if rollback {
		var previous controlRecord
		if err := json.Unmarshal(old, &previous); err != nil || previous.Cluster != r.record.Cluster || previous.Binding != r.record.Binding {
			r.mu.Unlock()
			return ErrIdentityMismatch
		}
		for domain, m := range previous.Domains {
			current := r.record.Domains[domain]
			if current == nil || current.Resource != m.Resource || current.Primary != m.Primary || current.Standby != m.Standby || current.Active != m.Active {
				r.mu.Unlock()
				return ErrIdentityMismatch
			}
		}
	}
	domains := append([]Domain(nil), r.record.RestoreDomains...)
	r.mu.Unlock()
	positions := map[Domain]replicaPosition{}
	for _, domain := range domains {
		db := domainFrom(dbs, domain)
		p, err := position(ctx, db.current.reader)
		if err != nil {
			return err
		}
		positions[domain] = p
		if err = db.offlineGuard(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE replica_state SET capture_mode='source',ready=1 WHERE id=1")
			return err
		}); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record.Pending != operationID {
		return ErrRestorePending
	}
	for domain, p := range positions {
		m := r.record.Domains[domain]
		m.Epoch = p.Epoch
		m.Ready = false
		m.State = "rebuild_required"
	}
	r.record.Pending = ""
	r.record.Finished = finished
	r.record.RestoreDomains = nil
	r.record.Revision++
	if err := r.persistLocked(ctx); err != nil {
		return err
	}
	for domain, p := range positions {
		db := domainFrom(dbs, domain)
		db.replica.epoch = p.Epoch
		db.setRestoring(false)
	}
	return nil
}
