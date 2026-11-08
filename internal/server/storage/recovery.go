package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/securefs"
)

var (
	ErrConfirmationStale = errors.New("storage_confirmation_stale")
	ErrManualRequired    = errors.New("storage_manual_required")
	ErrRecoveryNotFound  = errors.New("storage_operation_not_found")
	ErrRecoveryBusy      = errors.New("storage_busy")
)

type RecoveryRequest struct {
	Domain Domain `json:"domain"`
	Action string `json:"action"`
	Target string `json:"target"`
}
type RecoveryEndpoint struct {
	Role       string `json:"role"`
	Driver     string `json:"driver"`
	Label      string `json:"label"`
	InstanceID string `json:"instance_id"`
	Reachable  bool   `json:"reachable"`
	Ready      bool   `json:"ready"`
}
type RecoveryAction struct {
	Action string `json:"action"`
	Target string `json:"target"`
}
type RecoveryOperation struct {
	ID         string  `json:"id"`
	Action     string  `json:"action"`
	Target     string  `json:"target"`
	Status     string  `json:"status"`
	Phase      *string `json:"phase"`
	RowsCopied int64   `json:"rows_copied"`
	ErrorCode  string  `json:"error_code"`
}
type RecoveryPreview struct {
	OperationID                string            `json:"operation_id"`
	ExpiresAt                  time.Time         `json:"expires_at"`
	Revision                   uint64            `json:"revision"`
	Source                     *RecoveryEndpoint `json:"source"`
	Target                     RecoveryEndpoint  `json:"target"`
	Confirmation               string            `json:"confirmation"`
	RequiresDataLossAcceptance bool              `json:"requires_data_loss_acceptance"`
}
type RecoveryDomain struct {
	Domain            Domain             `json:"domain"`
	State             string             `json:"state"`
	ActiveRole        *string            `json:"active_role"`
	Primary           RecoveryEndpoint   `json:"primary"`
	Standby           *RecoveryEndpoint  `json:"standby"`
	ReplicationStatus string             `json:"replication_status"`
	LastSyncedAt      *time.Time         `json:"last_synced_at"`
	ProvenLossSeconds *float64           `json:"proven_loss_seconds"`
	FailureSince      *time.Time         `json:"failure_since"`
	ManualDeadlineAt  *time.Time         `json:"manual_deadline_at"`
	ManualRequired    bool               `json:"manual_required"`
	ReasonCode        string             `json:"reason_code"`
	Operation         *RecoveryOperation `json:"operation"`
	AvailableActions  []RecoveryAction   `json:"available_actions"`
}
type RecoveryStatus struct {
	Enabled              bool                        `json:"enabled"`
	Revision             uint64                      `json:"revision"`
	ServerTime           time.Time                   `json:"server_time"`
	ObservedAt           *time.Time                  `json:"observed_at,omitempty"`
	NotificationSettings StorageNotificationSettings `json:"notification_settings"`
	Domains              []RecoveryDomain            `json:"domains"`
}
type recoveryOperationRecord struct {
	RecoveryOperation
	Domain                                                                           Domain
	Actor, SourceRole, SourceInstance, TargetInstance, Epoch, Incident, Confirmation string
	Revision                                                                         uint64
	ExpiresAt, CreatedAt, FinishedAt                                                 time.Time
	AcceptDataLoss                                                                   bool
	AppliedEpoch                                                                     string
	Replaced                                                                         bool
}

func roleInstance(m *memberRecord, role string) string {
	if role == "primary" {
		return m.Primary
	}
	return m.Standby
}
func endpointStatus(cfg config.DatabaseConfig, m *memberRecord, role string) RecoveryEndpoint {
	ep := cfg.Primary
	if role == "standby" && cfg.Standby != nil {
		ep = *cfg.Standby
	}
	driver := ep.Driver
	if driver == "" {
		driver = "sqlite"
	}
	result := RecoveryEndpoint{Role: role, Driver: driver, Label: driver + " " + role}
	if m != nil {
		result.InstanceID = roleInstance(m, role)
		if role == "primary" {
			result.Reachable = m.PrimaryReachable
		} else {
			result.Reachable = m.StandbyReachable
		}
		if role == m.Active {
			registered := m.PrimaryRegistered
			if role == "standby" {
				registered = m.StandbyRegistered
			}
			result.Ready = registered && m.State != "restore_pending" && m.State != "disabling" && m.Reason != "active_integrity_error"
		} else {
			result.Ready = m.Ready
		}
	}
	return result
}
func recoveryAllowed(m *memberRecord, action, target string, serving bool) bool {
	if m == nil || target == m.Active || (target != "primary" && target != "standby") || m.Intent != nil {
		return false
	}
	registered := m.StandbyRegistered
	if target == "primary" {
		registered = m.PrimaryRegistered
	}
	if !registered || m.Reason == "active_integrity_error" || (m.Reason == "replica_integrity_error" && action != "rebuild") {
		return false
	}
	switch action {
	case "rebuild":
		return serving
	case "resume":
		return serving && m.Manual && m.Incident != ""
	case "promote":
		reachable := m.StandbyReachable
		if target == "primary" {
			reachable = m.PrimaryReachable
		}
		return !serving && reachable && m.Freeze != nil && m.Ready && m.Rebuild == nil
	}
	return false
}
func recoveryPending(record *controlRecord, domain Domain, now time.Time) *recoveryOperationRecord {
	for _, op := range record.Operations {
		if op.Domain == domain && !op.Replaced && (op.Status == "queued" || op.Status == "running" || (op.Status == "awaiting_confirmation" && now.Before(op.ExpiresAt))) {
			return op
		}
	}
	return nil
}
func (r *replication) recoveryStatusLocked(now freezePoint, dbs *Databases) RecoveryStatus {
	r.defaultOutage()
	settings := r.record.Outage.Snapshot
	mainMember := r.record.Domains[MainDomain]
	snapshotReady := settings.Ready && !r.record.Disabled && mainMember != nil && r.record.Outage.Token.Generation == mainMember.Generation
	s := RecoveryStatus{Enabled: !r.record.Disabled, Revision: r.record.Revision, ServerTime: now.Wall, NotificationSettings: StorageNotificationSettings{StorageNotificationPreferences: outagePreferences(settings.Preferences), SnapshotReady: snapshotReady}, Domains: []RecoveryDomain{}}
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		m := r.record.Domains[domain]
		if m == nil {
			continue
		}
		primary, standby := endpointStatus(r.cfg, m, "primary"), endpointStatus(r.cfg, m, "standby")
		serving := m.Freeze == nil && ((m.Active == "primary" && primary.Reachable) || (m.Active == "standby" && standby.Reachable))
		if dbs != nil {
			if d := domainFrom(dbs, domain); d != nil {
				serving = d.Ready()
				if m.Active == "primary" {
					primary.Reachable = serving
				} else {
					standby.Reachable = serving
				}
			}
		}
		state := m.State
		switch state {
		case "rebuild_required", "error":
			if m.Manual {
				state = "manual_required"
			} else if serving {
				state = "healthy"
			} else {
				state = "unavailable"
			}
		case "restore_pending", "disabling":
			state = "unavailable"
		}
		if m.Manual {
			state = "manual_required"
		}
		ds := RecoveryDomain{Domain: domain, State: state, Primary: primary, Standby: &standby, ManualRequired: m.Manual, ReasonCode: m.Reason, ReplicationStatus: m.ReplicationStatus, AvailableActions: []RecoveryAction{}, LastSyncedAt: m.LastSyncedAt}
		if m.Active == "primary" || m.Active == "standby" {
			role := m.Active
			ds.ActiveRole = &role
		}
		if ds.ReplicationStatus == "" {
			if !m.Ready {
				ds.ReplicationStatus = "initializing"
			} else {
				ds.ReplicationStatus = "caught_up"
			}
		}
		if m.Reason == "replica_integrity_error" || m.Reason == "active_integrity_error" {
			ds.ReplicationStatus = "error"
		}
		if d := r.domains[domain]; d != nil {
			if m.Ready && d.head.Load() > d.applied.Load() && ds.ReplicationStatus != "offline" && ds.ReplicationStatus != "error" {
				ds.ReplicationStatus = "lagging"
			}
			if stamp := d.lastSynced.Load(); stamp > 0 && (ds.LastSyncedAt == nil || stamp > ds.LastSyncedAt.UnixNano()) {
				value := time.Unix(0, stamp).UTC()
				ds.LastSyncedAt = &value
			}
		}
		if m.LossNanos != nil {
			loss := float64(*m.LossNanos) / 1e9
			if loss < 0 {
				loss = 0
			}
			ds.ProvenLossSeconds = &loss
		}
		if ds.ProvenLossSeconds == nil && m.Freeze != nil && m.Ready && m.LastAcknowledged != nil && m.LastAcknowledged.Epoch == m.Epoch && m.LastBarrier != nil && m.LastBarrier.Boot == m.Freeze.Boot && now.Boot == m.Freeze.Boot && m.LastBarrier.Nanos >= 0 && m.LastBarrier.Nanos <= m.Freeze.Nanos {
			loss := float64(m.Freeze.Nanos-m.LastBarrier.Nanos) / 1e9
			ds.ProvenLossSeconds = &loss
		}
		if ds.ReplicationStatus == "lagging" && m.LastBarrier != nil && now.Boot == m.LastBarrier.Boot && now.Nanos >= m.LastBarrier.Nanos && now.Nanos-m.LastBarrier.Nanos > int64(r.failoverConfig().MaxLossWindow) {
			ds.ReasonCode = "replica_lag_exceeded"
		}
		if !m.FailureSince.IsZero() {
			since := m.FailureSince
			deadline := since.Add(r.failoverConfig().ManualAfter)
			ds.FailureSince = &since
			ds.ManualDeadlineAt = &deadline
		}
		if op := recoveryPending(&r.record, domain, now.Wall); op != nil && op.Status != "awaiting_confirmation" {
			v := op.RecoveryOperation
			ds.Operation = &v
		}
		if r.record.Pending == "" && ds.Operation == nil {
			for _, action := range []string{"rebuild", "resume", "promote"} {
				target := opposite(m.Active)
				if recoveryAllowed(m, action, target, serving) {
					ds.AvailableActions = append(ds.AvailableActions, RecoveryAction{action, target})
				}
			}
		}
		s.Domains = append(s.Domains, ds)
	}
	return s
}
func standaloneStatus(cfg config.DatabaseConfig, dbs *Databases) RecoveryStatus {
	s := RecoveryStatus{ServerTime: time.Now().UTC(), Domains: []RecoveryDomain{}, NotificationSettings: StorageNotificationSettings{StorageNotificationPreferences: StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{}}}}
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		ep := endpointStatus(cfg, nil, "primary")
		state := "healthy"
		if dbs != nil {
			d := domainFrom(dbs, domain)
			ep.Reachable = d != nil && d.Ready()
			ep.Ready = ep.Reachable
			if !ep.Ready {
				state = "unavailable"
			}
			if d != nil {
				d.mu.Lock()
				if d.current != nil && d.current.dialect == MySQL {
					ep.Driver = "mysql"
					ep.Label = "mysql primary"
				}
				d.mu.Unlock()
			}
		}
		role := "primary"
		s.Domains = append(s.Domains, RecoveryDomain{Domain: domain, State: state, ActiveRole: &role, Primary: ep, ReplicationStatus: "unconfigured", AvailableActions: []RecoveryAction{}})
	}
	return s
}
func (dbs *Databases) RecoveryStatus(ctx context.Context) (RecoveryStatus, error) {
	if err := ctx.Err(); err != nil {
		return RecoveryStatus{}, err
	}
	if dbs == nil {
		return RecoveryStatus{}, ErrUnavailable
	}
	r := dbs.replication
	if r == nil {
		return standaloneStatus(config.DatabaseConfig{}, dbs), nil
	}
	now, err := r.now()
	if err != nil {
		return RecoveryStatus{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recoveryStatusLocked(now, dbs), nil
}

// ReadRecoveryStatus 只读本机控制记录；ObservedAt 不是数据库健康证明。
func ReadRecoveryStatus(ctx context.Context, cfg config.DatabaseConfig, dataDir string) (RecoveryStatus, error) {
	now := time.Now().UTC()
	path := filepath.Join(dataDir, "storage-control.db")
	exists, err := securefs.ValidateFile(path)
	if err != nil {
		return RecoveryStatus{}, err
	}
	if cfg.Standby == nil && !exists {
		s := standaloneStatus(cfg, nil)
		s.ObservedAt = &now
		return s, nil
	}
	db, err := OpenReader(ctx, path)
	if err != nil {
		return RecoveryStatus{}, err
	}
	defer db.Close()
	var raw []byte
	if err = db.QueryRowContext(ctx, "SELECT record FROM storage_control WHERE id=1").Scan(&raw); err != nil {
		return RecoveryStatus{}, safeFailure(err)
	}
	r := &replication{cfg: cfg}
	if err = json.Unmarshal(raw, &r.record); err != nil || r.record.Cluster == "" {
		return RecoveryStatus{}, ErrIdentityMismatch
	}
	if cfg.Standby == nil {
		var endpoints []json.RawMessage
		if json.Unmarshal([]byte(r.record.Binding), &endpoints) != nil || !r.record.Disabled || len(endpoints) != 2 || "["+string(endpoints[0])+"]" != configBinding(cfg) {
			return RecoveryStatus{}, ErrIdentityMismatch
		}
	}
	if cfg.Standby != nil && r.record.Binding != configBinding(cfg) {
		return RecoveryStatus{}, ErrIdentityMismatch
	}
	if r.record.Disabled {
		s := standaloneStatus(cfg, nil)
		s.ObservedAt = &now
		return s, nil
	}
	s := r.recoveryStatusLocked(freezePoint{Wall: now}, nil)
	s.ObservedAt = &now
	return s, nil
}

// OpenRecoveryDatabases 只打开已登记集群，不初始化新业务库或绕过待决恢复。
func OpenRecoveryDatabases(ctx context.Context, cfg config.DatabaseConfig, dataDir string) (*Databases, error) {
	if cfg.Standby == nil {
		return nil, ErrManualRequired
	}
	r, err := openControl(ctx, dataDir, false)
	if err != nil {
		return nil, err
	}
	r.cfg = cfg
	dbs := &Databases{replication: r}
	if r.record.Disabled || r.record.Binding != configBinding(cfg) || r.record.Pending != "" {
		dbs.Close()
		return nil, ErrRestorePending
	}
	for _, m := range r.record.Domains {
		registered := m.PrimaryRegistered
		if m.Active == "standby" {
			registered = m.StandbyRegistered
		}
		if !registered {
			dbs.Close()
			return nil, ErrIdentityMismatch
		}
	}
	if err = r.openRegistered(ctx, dbs, []Domain{MainDomain, HistoryDomain}); err != nil {
		dbs.Close()
		return nil, err
	}
	return dbs, nil
}
func (dbs *Databases) PreviewRecovery(ctx context.Context, actor string, req RecoveryRequest) (RecoveryPreview, error) {
	r := dbs.replication
	if r == nil || actor == "" {
		return RecoveryPreview{}, ErrManualRequired
	}
	d := r.domains[req.Domain]
	if d == nil {
		return RecoveryPreview{}, ErrManualRequired
	}
	if err := d.checkManual(ctx); err != nil {
		return RecoveryPreview{}, err
	}
	serving := d.source.Ready()
	if req.Action == "promote" && !serving {
		member := d.member()
		reachable := member.StandbyReachable
		registered := member.StandbyRegistered
		if req.Target == "primary" {
			reachable = member.PrimaryReachable
			registered = member.PrimaryRegistered
		}
		if !reachable && registered && member.Ready && member.Rebuild == nil && req.Target == opposite(member.Active) {
			if err := d.probeRecoveryCandidate(ctx, req.Target, member.Epoch); err != nil {
				return RecoveryPreview{}, err
			}
			serving = d.source.Ready()
		}
	}
	now, err := r.now()
	if err != nil {
		return RecoveryPreview{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.record.Domains[req.Domain]
	if r.record.Pending != "" || !recoveryAllowed(m, req.Action, req.Target, serving) {
		return RecoveryPreview{}, ErrManualRequired
	}
	if op := recoveryPending(&r.record, req.Domain, now.Wall); op != nil && op.Status != "awaiting_confirmation" {
		return RecoveryPreview{}, ErrRecoveryBusy
	}
	old := cloneRecoveryOperations(r.record.Operations)
	if r.record.Operations == nil {
		r.record.Operations = map[string]*recoveryOperationRecord{}
	}
	for _, op := range r.record.Operations {
		if op.Domain == req.Domain && op.Status == "awaiting_confirmation" {
			op.Replaced = true
		}
	}
	id := uuid.NewString()
	target := roleInstance(m, req.Target)
	confirmation := string(req.Domain) + "/" + req.Target + "/" + target
	op := &recoveryOperationRecord{RecoveryOperation: RecoveryOperation{ID: id, Action: req.Action, Target: req.Target, Status: "awaiting_confirmation"}, Domain: req.Domain, Actor: actor, SourceRole: m.Active, SourceInstance: roleInstance(m, m.Active), TargetInstance: target, Epoch: m.Epoch, Incident: m.Incident, Confirmation: confirmation, Revision: r.record.Revision, CreatedAt: now.Wall, ExpiresAt: now.Wall.Add(5 * time.Minute)}
	r.record.Operations[id] = op
	if err = r.persistLocked(ctx); err != nil {
		r.record.Operations = old
		return RecoveryPreview{}, err
	}
	source := endpointStatus(r.cfg, m, m.Active)
	return RecoveryPreview{OperationID: id, ExpiresAt: op.ExpiresAt, Revision: op.Revision, Source: &source, Target: endpointStatus(r.cfg, m, req.Target), Confirmation: confirmation, RequiresDataLossAcceptance: req.Action == "promote"}, nil
}
func cloneRecoveryOperations(ops map[string]*recoveryOperationRecord) map[string]*recoveryOperationRecord {
	result := make(map[string]*recoveryOperationRecord, len(ops))
	for id, op := range ops {
		v := *op
		result[id] = &v
	}
	return result
}
func (dbs *Databases) AcceptRecovery(ctx context.Context, actor, id, confirmation string, acceptDataLoss bool) (RecoveryOperation, error) {
	r := dbs.replication
	if r == nil {
		return RecoveryOperation{}, ErrRecoveryNotFound
	}
	now, err := r.now()
	if err != nil {
		return RecoveryOperation{}, err
	}
	for _, d := range r.domains {
		if err = d.checkManual(ctx); err != nil {
			return RecoveryOperation{}, err
		}
	}
	// 不在控制锁内等待业务锁或网络；准入状态只用于前提，执行前仍重验。
	serving := map[Domain]bool{}
	for domain, d := range r.domains {
		serving[domain] = d.source.Ready()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op := r.record.Operations[id]
	if op == nil || op.Actor != actor || (!op.FinishedAt.IsZero() && !now.Wall.Before(op.FinishedAt.Add(30*24*time.Hour))) {
		return RecoveryOperation{}, ErrRecoveryNotFound
	}
	if op.Status != "awaiting_confirmation" {
		return op.RecoveryOperation, nil
	}
	if !now.Wall.Before(op.ExpiresAt) {
		return RecoveryOperation{}, ErrRecoveryNotFound
	}
	if op.Replaced {
		return RecoveryOperation{}, ErrConfirmationStale
	}
	m := r.record.Domains[op.Domain]
	if confirmation != op.Confirmation || m == nil || op.Revision != r.record.Revision || op.Epoch != m.Epoch || op.SourceRole != m.Active || op.SourceInstance != roleInstance(m, m.Active) || op.TargetInstance != roleInstance(m, op.Target) || op.Incident != m.Incident || !recoveryAllowed(m, op.Action, op.Target, serving[op.Domain]) || r.record.Pending != "" {
		return RecoveryOperation{}, ErrConfirmationStale
	}
	if op.Action == "promote" && !acceptDataLoss {
		return RecoveryOperation{}, ErrManualRequired
	}
	old := *op
	phase := "validating"
	op.Status = "queued"
	op.Phase = &phase
	op.AcceptDataLoss = acceptDataLoss
	if err = r.persistLocked(ctx); err != nil {
		*op = old
		return RecoveryOperation{}, err
	}
	return op.RecoveryOperation, nil
}
func (dbs *Databases) RecoveryOperation(ctx context.Context, actor, id string) (RecoveryOperation, error) {
	r := dbs.replication
	if r == nil {
		return RecoveryOperation{}, ErrRecoveryNotFound
	}
	now, err := r.now()
	if err != nil {
		return RecoveryOperation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return RecoveryOperation{}, err
	}
	op := r.record.Operations[id]
	if op == nil || op.Actor != actor || op.Replaced || (op.Status == "awaiting_confirmation" && !now.Wall.Before(op.ExpiresAt)) || (!op.FinishedAt.IsZero() && !now.Wall.Before(op.FinishedAt.Add(30*24*time.Hour))) {
		return RecoveryOperation{}, ErrRecoveryNotFound
	}
	return op.RecoveryOperation, nil
}

func (d *replicaDomain) probeRecoveryCandidate(ctx context.Context, role, epoch string) error {
	candidate, err := d.openMember(ctx, role)
	if err != nil {
		return err
	}
	defer candidate.Close()
	phase, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	p, err := position(phase, candidate.current.reader)
	if err != nil {
		return err
	}
	if p.Epoch != epoch {
		return ErrIdentityMismatch
	}
	return d.update(ctx, func(m *memberRecord) {
		if role == "primary" {
			m.PrimaryReachable = true
		} else {
			m.StandbyReachable = true
		}
	})
}

// ReplicationEnabled 仅表示本实例持有尚未清退的 HA 控制器。
func (dbs *Databases) ReplicationEnabled() bool {
	if dbs == nil || dbs.replication == nil {
		return false
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.record.Disabled
}
