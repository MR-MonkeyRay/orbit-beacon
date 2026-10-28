// Package alerts owns rule validation, durable alert episodes and evaluation.
package alerts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

var (
	ErrNotFound = errors.New("alert rule not found")
	ErrCapacity = errors.New("maximum of 200 alert rules reached")
)

const MaxRules = 200

type Rule struct {
	ChannelIDs []string  `json:"channel_ids"`
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Enabled    bool      `json:"enabled"`
	ScopeKind  string    `json:"scope_kind"`
	ScopeID    *string   `json:"scope_id"`
	Metric     string    `json:"metric"`
	Comparator *string   `json:"comparator"`
	Threshold  *float64  `json:"threshold"`
	ForSeconds int       `json:"for_seconds"`
	Severity   string    `json:"severity"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Raw members distinguish omission from explicit null. Only scope_id,
// comparator and threshold accept null. POST requires nonnullable members.
type Patch struct {
	ChannelIDs json.RawMessage `json:"channel_ids"`
	Name       json.RawMessage `json:"name"`
	Enabled    json.RawMessage `json:"enabled"`
	ScopeKind  json.RawMessage `json:"scope_kind"`
	ScopeID    json.RawMessage `json:"scope_id"`
	Metric     json.RawMessage `json:"metric"`
	Comparator json.RawMessage `json:"comparator"`
	Threshold  json.RawMessage `json:"threshold"`
	ForSeconds json.RawMessage `json:"for_seconds"`
	Severity   json.RawMessage `json:"severity"`
}

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now}
}
func invalid(field, message string) error {
	return &nodes.ValidationError{Fields: map[string]string{field: message}}
}
func apply(r *Rule, p Patch, create bool) error {
	for _, f := range []struct {
		name     string
		raw      json.RawMessage
		dest     any
		nullable bool
	}{
		{"name", p.Name, &r.Name, false}, {"enabled", p.Enabled, &r.Enabled, false},
		{"scope_kind", p.ScopeKind, &r.ScopeKind, false}, {"scope_id", p.ScopeID, &r.ScopeID, true},
		{"metric", p.Metric, &r.Metric, false}, {"comparator", p.Comparator, &r.Comparator, true},
		{"threshold", p.Threshold, &r.Threshold, true}, {"for_seconds", p.ForSeconds, &r.ForSeconds, false},
		{"severity", p.Severity, &r.Severity, false},
	} {
		if len(f.raw) == 0 {
			if create && !f.nullable {
				return invalid(f.name, "is required")
			}
			continue
		}
		if !f.nullable && strings.TrimSpace(string(f.raw)) == "null" {
			return invalid(f.name, "cannot be null")
		}
		if err := json.Unmarshal(f.raw, f.dest); err != nil {
			return invalid(f.name, "has an invalid type")
		}
	}
	return nil
}

// ParseRuleSnapshot shares the evaluator's persisted rule representation.
// Scope IDs are checked syntactically, never against entities deleted since firing.
func ParseRuleSnapshot(raw []byte) (Rule, error) {
	var r Rule
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return r, invalid("rule_snapshot", "must be an object")
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	return r, ValidateRuleSyntax(r)
}

// ValidateRuleSyntax checks rule values without querying scope dependencies.
func ValidateRuleSyntax(r Rule) error {
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 100 {
		return invalid("name", "must contain 1..100 UTF-8 bytes")
	}
	if r.ForSeconds < 0 || r.ForSeconds > 86400 {
		return invalid("for_seconds", "must be between 0 and 86400")
	}
	if r.Severity != "info" && r.Severity != "warning" && r.Severity != "critical" {
		return invalid("severity", "must be info, warning or critical")
	}
	switch r.Metric {
	case "offline":
		if r.Comparator != nil || r.Threshold != nil {
			return invalid("metric", "offline has no comparator or threshold")
		}
		if time.Duration(r.ForSeconds)*time.Second < nodes.OnlineWindow {
			return invalid("for_seconds", "offline duration must be at least the online window")
		}
	case "cpu_percent", "memory_used_percent", "swap_used_percent", "disk_used_percent", "load1", "probes_down":
		bound := 100.0
		if r.Metric == "load1" {
			bound = 1e6
		} else if r.Metric == "probes_down" {
			bound = 5
		}
		if r.Threshold == nil || math.IsNaN(*r.Threshold) || math.IsInf(*r.Threshold, 0) || *r.Threshold < 0 || *r.Threshold > bound {
			return invalid("threshold", "must be finite and within the metric bounds")
		}
		if r.Comparator == nil || (*r.Comparator != ">" && *r.Comparator != "<") {
			return invalid("comparator", "must be > or <")
		}
	default:
		return invalid("metric", "unsupported metric")
	}
	if r.ScopeKind == "all" {
		if r.ScopeID != nil {
			return invalid("scope_id", "must be null for all")
		}
		return nil
	}
	switch r.ScopeKind {
	case "group", "node":
	default:
		return invalid("scope_kind", "must be all, group or node")
	}
	if r.ScopeID == nil || !identity.ValidUUID(*r.ScopeID) {
		return invalid("scope_id", "must identify an existing scope")
	}
	return nil
}

func validate(ctx context.Context, tx *sql.Tx, r Rule) error {
	if err := ValidateRuleSyntax(r); err != nil {
		return err
	}
	if r.ScopeKind == "all" {
		return nil
	}
	table := "nodes"
	if r.ScopeKind == "group" {
		table = "groups"
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE id=?", *r.ScopeID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return invalid("scope_id", "scope does not exist")
	}
	return nil
}

const ruleColumns = "id,name,enabled,scope_kind,scope_id,metric,comparator,threshold,for_seconds,severity,created_at,updated_at"

type scanner interface{ Scan(...any) error }

func scanRule(row scanner) (Rule, error) {
	var r Rule
	var created, updated string
	err := row.Scan(&r.ID, &r.Name, &r.Enabled, &r.ScopeKind, &r.ScopeID, &r.Metric, &r.Comparator, &r.Threshold, &r.ForSeconds, &r.Severity, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt, err = timekey.Parse(created)
	if err != nil {
		return r, err
	}
	r.UpdatedAt, err = timekey.Parse(updated)
	return r, err
}
func (s *Service) List(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+ruleColumns+" FROM alert_rules ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Rule, 0)
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range result {
		result[i].ChannelIDs, err = ruleChannels(ctx, s.db, result[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (s *Service) Get(ctx context.Context, id string) (Rule, error) {
	r, err := scanRule(s.db.QueryRowContext(ctx, "SELECT "+ruleColumns+" FROM alert_rules WHERE id=?", id))
	if err == nil {
		r.ChannelIDs, err = ruleChannels(ctx, s.db, id)
	}
	return r, err
}
func (s *Service) Save(ctx context.Context, id string, p Patch) (Rule, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Rule{}, err
	}
	defer tx.Rollback()
	create := id == ""
	var r Rule
	if create {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM alert_rules").Scan(&count); err != nil {
			return r, err
		}
		if count >= MaxRules {
			return r, ErrCapacity
		}
		r.ID, err = identity.UUID(rand.Reader)
		if err != nil {
			return r, err
		}
		r.CreatedAt = s.now().UTC()
	} else {
		r, err = scanRule(tx.QueryRowContext(ctx, "SELECT "+ruleColumns+" FROM alert_rules WHERE id=?", id))
		if err != nil {
			return r, err
		}
	}
	if err = apply(&r, p, create); err != nil {
		return Rule{}, err
	}
	if err = validate(ctx, tx, r); err != nil {
		return Rule{}, err
	}
	updated := s.now().UTC()
	if !create && !updated.After(r.UpdatedAt) {
		updated = r.UpdatedAt.Add(time.Nanosecond)
	}
	r.UpdatedAt = updated
	if create {
		_, err = tx.ExecContext(ctx, "INSERT INTO alert_rules("+ruleColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", r.ID, r.Name, r.Enabled, r.ScopeKind, r.ScopeID, r.Metric, r.Comparator, r.Threshold, r.ForSeconds, r.Severity, timekey.Format(r.CreatedAt), timekey.Format(r.UpdatedAt))
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE alert_rules SET name=?,enabled=?,scope_kind=?,scope_id=?,metric=?,comparator=?,threshold=?,for_seconds=?,severity=?,updated_at=? WHERE id=?", r.Name, r.Enabled, r.ScopeKind, r.ScopeID, r.Metric, r.Comparator, r.Threshold, r.ForSeconds, r.Severity, timekey.Format(r.UpdatedAt), id)
	}
	if err != nil {
		return Rule{}, err
	}
	if len(p.ChannelIDs) > 0 {
		if strings.TrimSpace(string(p.ChannelIDs)) == "null" || json.Unmarshal(p.ChannelIDs, &r.ChannelIDs) != nil || len(r.ChannelIDs) > 20 {
			return Rule{}, invalid("channel_ids", "must be an array of at most 20 existing channel IDs")
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM alert_rule_channels WHERE rule_id=?", r.ID); err != nil {
			return Rule{}, err
		}
		seen := map[string]bool{}
		for _, channelID := range r.ChannelIDs {
			if !identity.ValidUUID(channelID) || seen[channelID] {
				return Rule{}, invalid("channel_ids", "must contain unique existing channel IDs")
			}
			seen[channelID] = true
			var exists int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM notification_channels WHERE id=?", channelID).Scan(&exists); err != nil {
				return Rule{}, err
			}
			if exists == 0 {
				return Rule{}, invalid("channel_ids", "channel does not exist")
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO alert_rule_channels(rule_id,channel_id) VALUES(?,?)", r.ID, channelID); err != nil {
				return Rule{}, err
			}
		}
	}
	r.ChannelIDs, err = ruleChannels(ctx, tx, r.ID)
	if err != nil {
		return Rule{}, err
	}
	action := "alert_rule.update"
	if create {
		action = "alert_rule.create"
	}
	if err = audit.Insert(ctx, tx, action, r.ID, r.UpdatedAt); err != nil {
		return Rule{}, err
	}
	return r, tx.Commit()
}
func (s *Service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "DELETE FROM alert_rules WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if err = audit.Insert(ctx, tx, "alert_rule.delete", id, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

// Routing is saved with the rule and its audit record under the same writer lock.
func ruleChannels(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT channel_id FROM alert_rule_channels WHERE rule_id=? ORDER BY channel_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
