// Package commands owns the closed remote-command queue. Callers must fence the
// authenticated report identity before invoking Exchange inside its transaction.
package commands

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

const (
	Lifetime        = 5 * time.Minute
	UpgradeLifetime = 20 * time.Minute
	Retention       = 90 * 24 * time.Hour
)

var (
	ErrNotFound        = errors.New("command or node not found")
	ErrSameVersion     = errors.New("node already runs target version")
	ErrInvalid         = errors.New("invalid command request")
	ErrNotAllowed      = errors.New("command not allowed")
	ErrActive          = errors.New("command active")
	ErrNodeUnavailable = errors.New("node unavailable")
	ErrNotPending      = errors.New("command is not pending")
)

// StoppedSQL is a correlated scalar expression, shared by inventory and alerts.
// A stop only describes the process it was delivered to: any later run changes
// nodes.run_generation, so a recovered result or a crash can never hide a real
// outage. Generations restart with a new credential, so the epoch must match
// too. The latest stop must itself have succeeded.
const StoppedSQL = `COALESCE((SELECT state='succeeded' AND credential_epoch=nodes.credential_epoch AND delivered_generation=nodes.run_generation FROM agent_commands WHERE node_id=nodes.id AND kind='stop' ORDER BY created_at DESC,id DESC LIMIT 1),0)`

type Record struct {
	v1.Command
	NodeID          string     `json:"node_id"`
	CredentialEpoch int64      `json:"-"`
	State           string     `json:"state"`
	ResultCode      string     `json:"result_code"`
	RequestedBy     string     `json:"requested_by"`
	CreatedAt       time.Time  `json:"created_at"`
	DeliveredAt     *time.Time `json:"delivered_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

type Service struct {
	db  *storage.Database
	now func() time.Time
}

func New(db *storage.Database, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db, now}
}

type CreateRequest struct {
	Kind           string `json:"kind"`
	TargetVersion  string `json:"target_version"`
	Confirm        string `json:"confirm"`
	targetProvided bool
}

// UnmarshalJSON 保留字段是否出现，restart/stop 不接受空值或 null 的目标版本。
func (r *CreateRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Kind          string          `json:"kind"`
		TargetVersion json.RawMessage `json:"target_version"`
		Confirm       string          `json:"confirm"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*r = CreateRequest{Kind: wire.Kind, Confirm: wire.Confirm, targetProvided: wire.TargetVersion != nil}
	if r.targetProvided {
		return json.Unmarshal(wire.TargetVersion, &r.TargetVersion)
	}
	return nil
}

func (r CreateRequest) valid() bool {
	switch r.Kind {
	case v1.RemoteCommandUpgrade:
		_, err := v1.ParseVersion(r.TargetVersion)
		return err == nil
	case v1.RemoteCommandRestart, v1.RemoteCommandStop:
		return !r.targetProvided && r.TargetVersion == ""
	default:
		return false
	}
}

func (s *Service) Create(ctx context.Context, nodeID, actor string, req CreateRequest) (Record, error) {
	if !uuid.ValidUUID(nodeID) || !req.valid() {
		return Record{}, ErrInvalid
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return Record{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	var name, status, raw, generation, session, version string
	var epoch int64
	var credential sql.NullBool
	var stopped bool
	err = tx.QueryRowContext(ctx, `SELECT name,status,agent_runtime,credential_epoch,length(token_hash)>0,run_generation,report_session_id,CASE WHEN json_valid(system) THEN COALESCE(json_extract(system,'$.agent_version'),'') ELSE '' END,`+StoppedSQL+` FROM nodes WHERE id=?`, nodeID).Scan(&name, &status, &raw, &epoch, &credential, &generation, &session, &version, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	if req.Kind == v1.RemoteCommandStop && req.Confirm != name {
		return Record{}, ErrInvalid
	}
	if status == "disabled" || !credential.Bool || stopped {
		return Record{}, ErrNodeUnavailable
	}
	runtime, decodeErr := v1.DecodeRuntimeSettings(raw)
	if decodeErr != nil || generation == "0" || session == "" || !runtime.AllowsCommand(req.Kind) {
		return Record{}, ErrNotAllowed
	}
	if req.Kind == v1.RemoteCommandUpgrade && version == req.TargetVersion {
		return Record{}, ErrSameVersion
	}
	lifetime := Lifetime
	if req.Kind == v1.RemoteCommandUpgrade {
		lifetime = UpgradeLifetime
	}
	now := s.now().UTC()
	if err := expire(ctx, tx, nodeID, now); err != nil {
		return Record{}, err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_commands WHERE node_id=? AND state IN ('pending','delivered'))`, nodeID).Scan(&active); err != nil {
		return Record{}, err
	}
	if active {
		return Record{}, ErrActive
	}
	id, err := uuid.UUID(rand.Reader)
	if err != nil {
		return Record{}, err
	}
	record := Record{Command: v1.Command{ID: id, Kind: req.Kind, TargetVersion: req.TargetVersion, ExpiresAt: now.Add(lifetime)}, NodeID: nodeID, CredentialEpoch: epoch, State: "pending", RequestedBy: actor, CreatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_commands(id,node_id,credential_epoch,kind,target_version,state,requested_by,created_at,expires_at) VALUES(?,?,?,?,?,'pending',?,?,?)`, id, nodeID, epoch, req.Kind, req.TargetVersion, actor, timekey.Format(now), timekey.Format(record.ExpiresAt))
	if err != nil {
		return Record{}, err
	}
	if err := audit.InsertDetails(ctx, tx, "command.create", nodeID, now, audit.Details{"command_id": id, "kind": req.Kind, "target_version": req.TargetVersion}); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return record, nil
}

type BulkResult struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"`
}

// CreateGroup snapshots membership before creating each command in its own transaction.
func (s *Service) CreateGroup(ctx context.Context, groupID, actor string, req CreateRequest) (BulkResult, error) {
	var result BulkResult
	if !req.valid() {
		return result, ErrInvalid
	}
	var name string
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return result, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if err := lease.QueryRowContext(ctx, `SELECT name FROM groups WHERE id=?`, groupID).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrNotFound
		}
		return result, err
	}
	if req.Confirm != name {
		return result, ErrInvalid
	}
	rows, err := lease.QueryContext(ctx, `SELECT id,name FROM nodes WHERE group_id=? ORDER BY id`, groupID)
	if err != nil {
		return result, err
	}
	type target struct{ id, name string }
	var targets []target
	for rows.Next() {
		var n target
		if err := rows.Scan(&n.id, &n.name); err != nil {
			rows.Close()
			return result, err
		}
		targets = append(targets, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, n := range targets {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if _, err := s.Create(ctx, n.id, actor, CreateRequest{Kind: req.Kind, TargetVersion: req.TargetVersion, Confirm: n.name}); err != nil {
			result.Skipped++
		} else {
			result.Created++
		}
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := audit.InsertDetails(ctx, tx, "command.bulk", groupID, s.now().UTC(), audit.Details{
		"group_id": groupID, "kind": req.Kind, "target_version": req.TargetVersion, "created": result.Created, "skipped": result.Skipped,
	}); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Service) Cancel(ctx context.Context, nodeID, id string) error {
	if !uuid.ValidUUID(id) {
		return ErrInvalid
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM agent_commands WHERE id=? AND node_id=?`, id, nodeID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "pending" {
		return ErrNotPending
	}
	if err := transition(ctx, tx, nodeID, id, "canceled", "", s.now().UTC(), false); err != nil {
		return err
	}
	return tx.Commit()
}

const columns = `id,kind,target_version,expires_at,node_id,credential_epoch,state,result_code,requested_by,created_at,delivered_at,finished_at`

func (s *Service) List(ctx context.Context, nodeID string) ([]Record, error) {
	var exists bool
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if err := lease.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=?)`, nodeID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := lease.QueryContext(ctx, `SELECT `+columns+` FROM agent_commands WHERE node_id=? ORDER BY created_at DESC,id DESC LIMIT 20`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now().UTC()
	result := []Record{}
	for rows.Next() {
		var r Record
		var expires, created string
		var delivered, finished *string
		if err := rows.Scan(&r.ID, &r.Kind, &r.TargetVersion, &expires, &r.NodeID, &r.CredentialEpoch, &r.State, &r.ResultCode, &r.RequestedBy, &created, &delivered, &finished); err != nil {
			return nil, err
		}
		for _, p := range []struct {
			raw string
			to  *time.Time
		}{{expires, &r.ExpiresAt}, {created, &r.CreatedAt}} {
			v, err := timekey.Parse(p.raw)
			if err != nil {
				return nil, err
			}
			*p.to = v
		}
		for _, p := range []struct {
			raw *string
			to  **time.Time
		}{{delivered, &r.DeliveredAt}, {finished, &r.FinishedAt}} {
			if p.raw != nil {
				v, err := timekey.Parse(*p.raw)
				if err != nil {
					return nil, err
				}
				*p.to = &v
			}
		}
		// 过期状态由后续上报或周期清理落库，读取时不能继续显示为进行中。
		if (r.State == "pending" || r.State == "delivered") && !now.Before(r.ExpiresAt) {
			r.State, r.ResultCode = "expired", "expired"
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func transition(ctx context.Context, tx *storage.Tx, nodeID, id, state, code string, now time.Time, delivering bool) error {
	query := `UPDATE agent_commands SET state=?,result_code=?,finished_at=? WHERE id=? AND state IN ('pending','delivered')`
	if delivering {
		query = `UPDATE agent_commands SET state=?,result_code=?,delivered_at=? WHERE id=? AND state='pending'`
	}
	result, err := tx.ExecContext(ctx, query, state, code, timekey.Format(now), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	// 下发只记在 delivered_at：审计保留谁发起和结果如何，不随上报刷屏。
	if err != nil || count == 0 || delivering {
		return err
	}
	return audit.InsertDetails(ctx, tx, "command."+state, nodeID, now, audit.Details{"command_id": id, "result_code": code})
}
func transitionMatching(ctx context.Context, tx *storage.Tx, nodeID, condition, state, code string, now time.Time, args ...any) error {
	values := append([]any{nodeID}, args...)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM agent_commands WHERE node_id=? AND state IN ('pending','delivered') AND `+condition, values...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := transition(ctx, tx, nodeID, id, state, code, now, false); err != nil {
			return err
		}
	}
	return nil
}
func expire(ctx context.Context, tx *storage.Tx, nodeID string, now time.Time) error {
	return transitionMatching(audit.WithSystemActor(ctx, "server"), tx, nodeID, `expires_at<=?`, "expired", "expired", now, timekey.Format(now))
}

// CancelActive is used by identity/disable transactions, including credential reset.
func CancelActive(ctx context.Context, tx *storage.Tx, nodeID string, now time.Time) error {
	return transitionMatching(audit.WithSystemActor(ctx, "server"), tx, nodeID, "1=1", "canceled", "", now)
}

// Exchange applies results before recording the delivery, so a result recovered
// by a new process stays bound to the generation that received the command.
func Exchange(ctx context.Context, tx *storage.Tx, nodeID string, epoch int64, generation string, results []v1.CommandResult, now time.Time) (*v1.Command, error) {
	ctx = audit.WithSystemActor(ctx, "agent")
	if len(results) > 2 {
		return nil, ErrInvalid
	}
	for _, r := range results {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM agent_commands WHERE id=? AND node_id=? AND credential_epoch=? AND state='delivered'`, r.ID, nodeID, epoch).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := transition(ctx, tx, nodeID, id, r.Status, r.Code, now, false); err != nil {
			return nil, err
		}
	}
	if err := expire(ctx, tx, nodeID, now); err != nil {
		return nil, err
	}
	if err := transitionMatching(audit.WithSystemActor(ctx, "server"), tx, nodeID, "credential_epoch<>?", "canceled", "", now, epoch); err != nil {
		return nil, err
	}
	var c v1.Command
	var expiry, state string
	err := tx.QueryRowContext(ctx, `SELECT id,kind,target_version,expires_at,state FROM agent_commands WHERE node_id=? AND state IN ('pending','delivered')`, nodeID).Scan(&c.ID, &c.Kind, &c.TargetVersion, &expiry, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.ExpiresAt, err = timekey.Parse(expiry)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if state == "pending" {
		if err := transition(ctx, tx, nodeID, c.ID, "delivered", "", now, true); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_commands SET delivered_generation=? WHERE id=?`, generation, c.ID); err != nil {
		return nil, err
	}
	return &c, nil
}

// Cleanup also expires commands for agents that have already exited. Each call
// holds one bounded transaction and preserves the newest record for every node.
func (s *Service) Cleanup(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, ErrInvalid
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return 0, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT node_id FROM agent_commands WHERE state IN ('pending','delivered') AND expires_at<=? LIMIT ?`, timekey.Format(now), limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := expire(ctx, tx, id, now); err != nil {
			return 0, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM agent_commands WHERE id IN (SELECT c.id FROM agent_commands c WHERE c.state NOT IN ('pending','delivered') AND c.finished_at<? AND c.id<>(SELECT n.id FROM agent_commands n WHERE n.node_id=c.node_id ORDER BY n.created_at DESC,n.id DESC LIMIT 1) ORDER BY c.finished_at LIMIT ?)`, timekey.Format(now.Add(-Retention)), limit)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
