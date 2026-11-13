package alerts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

// 所有事件判定都使用当前 nodes 行和同一 UTC 时刻，包含开始、不包含结束。
const activeSilenceSQL = `SELECT 1 FROM alert_silences s WHERE s.starts_at<=? AND s.ends_at>?
 AND (s.scope_kind='all' OR (s.scope_kind='node' AND s.scope_id=nodes.id)
 OR (s.scope_kind='group' AND s.scope_id=nodes.group_id))`

// releasableEventSQL 选出仍在触发、但已没有生效窗口的静默事件。
const releasableEventSQL = `state='firing' AND silenced=1
 AND NOT EXISTS(SELECT 1 FROM nodes WHERE nodes.id=alert_events.node_id AND EXISTS(` + activeSilenceSQL + `))`

type Silence struct {
	ID        string    `json:"id"`
	Reason    string    `json:"reason"`
	ScopeKind string    `json:"scope_kind"`
	ScopeID   *string   `json:"scope_id"`
	ScopeName string    `json:"scope_name"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	CreatedAt time.Time `json:"created_at"`
	Active    bool      `json:"active"`
}

type SilenceRequest struct {
	Reason    json.RawMessage `json:"reason"`
	ScopeKind json.RawMessage `json:"scope_kind"`
	ScopeID   json.RawMessage `json:"scope_id"`
	StartsAt  json.RawMessage `json:"starts_at"`
	EndsAt    json.RawMessage `json:"ends_at"`
}

// ValidateSilenceSyntax 同时用于 API 和备份；历史窗口无需晚于恢复时刻。
func ValidateSilenceSyntax(s Silence) error {
	if strings.TrimSpace(s.Reason) == "" || len(s.Reason) > 255 {
		return validation.Invalid("reason", "must contain 1..255 UTF-8 bytes")
	}
	switch s.ScopeKind {
	case "all":
		if s.ScopeID != nil {
			return validation.Invalid("scope_id", "must be null for all")
		}
	case "group", "node", "service_group":
		if s.ScopeID == nil || !uuid.ValidUUID(*s.ScopeID) {
			return validation.Invalid("scope_id", "must identify an existing scope")
		}
	default:
		return validation.Invalid("scope_kind", "must be all, group, node or service_group")
	}
	if s.StartsAt.IsZero() {
		return validation.Invalid("starts_at", "must be an RFC3339 timestamp")
	}
	if !s.EndsAt.After(s.StartsAt) {
		return validation.Invalid("ends_at", "must be after starts_at and now")
	}
	if s.EndsAt.Sub(s.StartsAt) > 30*24*time.Hour {
		return validation.Invalid("ends_at", "must be within 30 days of starts_at")
	}
	return nil
}

func (s *Service) CreateSilence(ctx context.Context, req SilenceRequest) (Silence, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return Silence{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	now := s.now().UTC()
	silence := Silence{StartsAt: now, CreatedAt: now}
	for _, f := range []struct {
		name               string
		raw                json.RawMessage
		dest               any
		required, nullable bool
	}{
		{"reason", req.Reason, &silence.Reason, true, false},
		{"scope_kind", req.ScopeKind, &silence.ScopeKind, true, false},
		{"scope_id", req.ScopeID, &silence.ScopeID, false, true},
		{"starts_at", req.StartsAt, &silence.StartsAt, false, false},
		{"ends_at", req.EndsAt, &silence.EndsAt, true, false},
	} {
		if len(f.raw) == 0 {
			if f.required {
				return Silence{}, validation.Invalid(f.name, "is required")
			}
			continue
		}
		if !f.nullable && strings.TrimSpace(string(f.raw)) == "null" {
			return Silence{}, validation.Invalid(f.name, "cannot be null")
		}
		if json.Unmarshal(f.raw, f.dest) != nil {
			return Silence{}, validation.Invalid(f.name, "has an invalid type")
		}
	}
	silence.Reason = strings.TrimSpace(silence.Reason)
	silence.StartsAt = silence.StartsAt.UTC()
	silence.EndsAt = silence.EndsAt.UTC()
	if err := ValidateSilenceSyntax(silence); err != nil {
		return Silence{}, err
	}
	if !silence.EndsAt.After(now) {
		return Silence{}, validation.Invalid("ends_at", "must be after starts_at and now")
	}
	id, err := uuid.UUID(rand.Reader)
	if err != nil {
		return Silence{}, err
	}
	silence.ID = id
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return Silence{}, err
	}
	defer tx.Rollback()
	if silence.ScopeKind != "all" {
		table := "nodes"
		if silence.ScopeKind == "group" {
			table = "`groups`"
		} else if silence.ScopeKind == "service_group" {
			table = "service_groups"
		}
		err = tx.QueryRowContext(ctx, "SELECT name FROM "+table+" WHERE id=?", *silence.ScopeID).Scan(&silence.ScopeName)
		if errors.Is(err, sql.ErrNoRows) {
			return Silence{}, validation.Invalid("scope_id", "scope does not exist")
		}
		if err != nil {
			return Silence{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES(?,?,?,?,?,?,?)`, id, silence.Reason, silence.ScopeKind, silence.ScopeID, timekey.Format(silence.StartsAt), timekey.Format(silence.EndsAt), timekey.Format(now))
	if err != nil {
		return Silence{}, err
	}
	if err = audit.Insert(ctx, tx, "alert_silence.create", id, now); err != nil {
		return Silence{}, err
	}
	silence.Active = !silence.StartsAt.After(now)
	return silence, tx.Commit()
}

func (s *Service) Silences(ctx context.Context) ([]Silence, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	now := s.now().UTC()
	rows, err := lease.QueryContext(ctx, `SELECT s.id,s.reason,s.scope_kind,s.scope_id,s.starts_at,s.ends_at,s.created_at,
 COALESCE(g.name,n.name,sg.name,'') FROM alert_silences s
	LEFT JOIN `+"`groups`"+` g ON s.scope_group_id=g.id LEFT JOIN nodes n ON s.scope_node_id=n.id
 LEFT JOIN service_groups sg ON s.scope_service_group_id=sg.id
 WHERE s.ends_at>? ORDER BY s.starts_at,s.id`, timekey.Format(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Silence, 0)
	for rows.Next() {
		var entry Silence
		var start, end, created string
		if err = rows.Scan(&entry.ID, &entry.Reason, &entry.ScopeKind, &entry.ScopeID, &start, &end, &created, &entry.ScopeName); err != nil {
			return nil, err
		}
		if entry.StartsAt, err = timekey.Parse(start); err != nil {
			return nil, err
		}
		if entry.EndsAt, err = timekey.Parse(end); err != nil {
			return nil, err
		}
		if entry.CreatedAt, err = timekey.Parse(created); err != nil {
			return nil, err
		}
		entry.Active = !entry.StartsAt.After(now)
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (s *Service) DeleteSilence(ctx context.Context, id string) error {
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
	result, err := tx.ExecContext(ctx, "DELETE FROM alert_silences WHERE id=?", id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	if err = audit.Insert(ctx, tx, "alert_silence.delete", id, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) CleanupSilences(ctx context.Context, before time.Time, limit int) (int64, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return 0, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if limit < 1 {
		return 0, validation.Invalid("limit", "must be positive")
	}
	tx, txErr := lease.BeginWrite(ctx)
	if txErr != nil {
		return 0, txErr
	}
	defer tx.Rollback()
	count, err := storage.DeleteBatch(ctx, tx, `SELECT id FROM alert_silences WHERE ends_at<? ORDER BY ends_at,id LIMIT ?`, `DELETE FROM alert_silences WHERE id=?`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, commitErr
	}
	return count, nil
}

// releasableSilences 先用读查询筛选。窗口生效期间不开写事务，否则全局静默时
// 每轮都有上千次空写与上报争抢写锁。
func (e *Evaluator) releasableSilences(ctx context.Context, open map[pair]openEvent, now time.Time) ([]openEvent, error) {
	lease, leaseErr := e.service.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	silenced := false
	for _, o := range open {
		silenced = silenced || o.Silenced
	}
	if !silenced {
		return nil, nil
	}
	stamp := timekey.Format(now)
	rows, err := lease.QueryContext(ctx, `SELECT id,rule_id,node_id FROM alert_events WHERE `+releasableEventSQL, stamp, stamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []openEvent
	for rows.Next() {
		var id string
		var key pair
		if err := rows.Scan(&id, &key.rule, &key.node); err != nil {
			return nil, err
		}
		_, retry := e.resolutions[key]
		if o, exists := open[key]; exists && o.ID == id && !retry {
			result = append(result, o)
		}
	}
	return result, rows.Err()
}

func (e *Evaluator) releaseSilence(ctx context.Context, o openEvent, now time.Time) error {
	lease, leaseErr := e.service.db.Acquire(ctx, storage.Writer)
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
	stamp := timekey.Format(now)
	result, err := tx.ExecContext(ctx, `UPDATE alert_events SET silenced=0 WHERE id=? AND `+releasableEventSQL, o.ID, stamp, stamp)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 1 {
		notification := Notification{EventID: o.ID, Rule: o.rule, NodeID: o.NodeID, NodeName: o.NodeName, State: "firing", Value: o.Value, StartedAt: o.StartedAt}
		if err = insertInbox(ctx, tx, notification, e.opts.Now()); err != nil {
			return err
		}
		if err = e.opts.Notifier.Enqueue(ctx, tx, notification); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if count == 1 {
		e.opts.Notifier.Wake()
	}
	return nil
}
