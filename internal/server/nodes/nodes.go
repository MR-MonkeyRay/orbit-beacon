package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"orbitbeacon/internal/identity"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/audit"
)

type serviceError string

func (err serviceError) Error() string { return string(err) }

const (
	ErrCapacityReached serviceError = "node capacity reached"
	ErrNotFound        serviceError = "node not found"
	ErrUnauthenticated serviceError = "node authentication required"
	ErrInvalidInput    serviceError = "invalid node request"
)

type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Notes     string    `json:"notes"`
	Region    string    `json:"region"`
	Provider  string    `json:"provider"`
	GroupID   *string   `json:"group_id"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateRequest struct {
	Name string `json:"name"`
}

type MetadataPatch struct {
	Name     *string `json:"name"`
	Notes    *string `json:"notes"`
	Region   *string `json:"region"`
	Provider *string `json:"provider"`
	// A nil raw message leaves the group unchanged; JSON null clears it.
	GroupID json.RawMessage `json:"group_id"`
	Tags    *[]string       `json:"tags"`
}

type BatchRequest struct {
	NodeIDs []string `json:"node_ids"`
	Action  string   `json:"action"`
	Confirm bool     `json:"confirm"`
}

type BatchResult struct {
	NodeID    string `json:"node_id"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}

type DesiredConfig struct {
	Revision              int64    `json:"revision"`
	ReportIntervalSeconds int      `json:"report_interval_seconds"`
	DiskIntervalSeconds   int      `json:"disk_interval_seconds"`
	Interfaces            []string `json:"interfaces"`
	DiskPaths             []string `json:"disk_paths"`
	ProbeTargets          []string `json:"probe_targets"`
	BillingDay            int      `json:"billing_day"`
	QuotaBytes            string   `json:"quota_bytes"`
}

type ConfigPatch struct {
	ReportIntervalSeconds *int      `json:"report_interval_seconds"`
	DiskIntervalSeconds   *int      `json:"disk_interval_seconds"`
	Interfaces            *[]string `json:"interfaces"`
	DiskPaths             *[]string `json:"disk_paths"`
	BillingDay            *int      `json:"billing_day"`
	QuotaBytes            *string   `json:"quota_bytes"`
}

type ValidationError struct{ Fields map[string]string }

func (err *ValidationError) Error() string { return "desired configuration validation failed" }

type Options struct {
	MaxNodes int
	Now      func() time.Time
	Random   io.Reader
}

type Service struct {
	db       *sql.DB
	maxNodes int
	now      func() time.Time
	random   io.Reader
	randomMu sync.Mutex
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	if opts.MaxNodes <= 0 {
		opts.MaxNodes = 500
	}
	return &Service{db: db, maxNodes: opts.MaxNodes, now: opts.Now, random: opts.Random}
}

// Database and Capacity keep default HTTP services on the same storage policy.
func (s *Service) Database() *sql.DB { return s.db }

func (s *Service) Capacity() int { return s.maxNodes }

func (s *Service) CreatePending(ctx context.Context, req CreateRequest) (Node, error) {
	if strings.TrimSpace(req.Name) == "" || len(req.Name) > 255 {
		return Node{}, ErrInvalidInput
	}
	s.randomMu.Lock()
	id, err := identity.UUID(s.random)
	s.randomMu.Unlock()
	if err != nil {
		return Node{}, err
	}
	node := Node{ID: id, Name: req.Name, Status: "pending", Tags: []string{}, CreatedAt: s.now().UTC()}
	// BEGIN IMMEDIATE reserves the writer before checking capacity, including
	// across independent services using the same persistent database.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes").Scan(&count); err != nil {
		return Node{}, err
	}
	if count >= s.maxNodes {
		return Node{}, ErrCapacityReached
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO nodes(id, name, status, created_at) VALUES (?, ?, ?, ?)", id, req.Name, node.Status, node.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return Node{}, err
	}
	if err := audit.Insert(ctx, tx, "node.create", id, s.now()); err != nil {
		return Node{}, err
	}
	if err := tx.Commit(); err != nil {
		return Node{}, err
	}
	return node, nil
}

type scanner interface{ Scan(...any) error }

const nodeColumns = `id, name, status, created_at, notes, region, provider, group_id,
	(SELECT json_group_array(tag) FROM (SELECT tag FROM node_tags WHERE node_id = nodes.id ORDER BY tag))`

func scanNode(row scanner) (Node, error) {
	var node Node
	var created, tags string
	if err := row.Scan(&node.ID, &node.Name, &node.Status, &created, &node.Notes, &node.Region, &node.Provider, &node.GroupID, &tags); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	var err error
	node.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Node{}, err
	}
	if err := json.Unmarshal([]byte(tags), &node.Tags); err != nil {
		return Node{}, err
	}
	if node.Tags == nil {
		node.Tags = []string{}
	}
	return node, err
}

func (s *Service) Get(ctx context.Context, id string) (Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = ?", id))
}

func (s *Service) List(ctx context.Context, limit, offset int) ([]Node, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+nodeColumns+" FROM nodes ORDER BY created_at, id LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Node, 0)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

// PatchMetadata changes only supplied metadata and replaces tags only when present.
func (s *Service) PatchMetadata(ctx context.Context, id string, patch MetadataPatch) (Node, error) {
	if !identity.ValidUUID(id) {
		return Node{}, ErrInvalidInput
	}
	fields := make(map[string]string)
	if patch.Name != nil && (strings.TrimSpace(*patch.Name) == "" || len(*patch.Name) > 255) {
		fields["name"] = "must be nonempty and at most 255 bytes"
	}
	var groupID *string
	if patch.GroupID != nil {
		if err := json.Unmarshal(patch.GroupID, &groupID); err != nil || (groupID != nil && !identity.ValidUUID(*groupID)) {
			fields["group_id"] = "must be a group UUID or null"
		}
	}
	if patch.Tags != nil {
		seen := make(map[string]bool, len(*patch.Tags))
		for _, tag := range *patch.Tags {
			if strings.TrimSpace(tag) == "" || len(tag) > 255 {
				fields["tags"] = "entries must be nonempty and at most 255 bytes"
			}
			if seen[tag] {
				fields["tags"] = "entries must be unique"
			}
			seen[tag] = true
		}
	}
	if len(fields) != 0 {
		return Node{}, &ValidationError{Fields: fields}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, err
	}
	defer tx.Rollback()
	if groupID != nil {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM groups WHERE id = ?)", *groupID).Scan(&exists); err != nil {
			return Node{}, err
		}
		if !exists {
			return Node{}, &ValidationError{Fields: map[string]string{"group_id": "group does not exist"}}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET name = COALESCE(?, name), notes = COALESCE(?, notes), region = COALESCE(?, region), provider = COALESCE(?, provider), group_id = CASE WHEN ? THEN ? ELSE group_id END WHERE id = ?`, patch.Name, patch.Notes, patch.Region, patch.Provider, patch.GroupID != nil, groupID, id)
	if err := affected(result, err); err != nil {
		return Node{}, err
	}
	if patch.Tags != nil {
		if _, err := tx.ExecContext(ctx, "DELETE FROM node_tags WHERE node_id = ?", id); err != nil {
			return Node{}, err
		}
		for _, tag := range *patch.Tags {
			if _, err := tx.ExecContext(ctx, "INSERT INTO node_tags(node_id, tag) VALUES (?, ?)", id, tag); err != nil {
				return Node{}, err
			}
		}
	}
	node, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = ?", id))
	if err != nil {
		return Node{}, err
	}
	if err := audit.Insert(ctx, tx, "node.metadata.patch", id, s.now()); err != nil {
		return Node{}, err
	}
	if err := tx.Commit(); err != nil {
		return Node{}, err
	}
	return node, nil
}

func (s *Service) SetDisabled(ctx context.Context, id string, disabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Enabling cannot invent an enrolled identity: tokenless assets stay pending.
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET status = CASE WHEN ? THEN 'disabled' WHEN token_hash IS NULL THEN 'pending' ELSE 'unknown' END WHERE id = ?`, disabled, id)
	if err := affected(result, err); err != nil {
		return err
	}
	action := "node.enable"
	if disabled {
		action = "node.disable"
	}
	if err := audit.Insert(ctx, tx, action, id, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func affected(result sql.Result, err error) error {
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
	return nil
}

func (s *Service) AuthenticateToken(ctx context.Context, token string) (Node, error) {
	if len(token) < 32 || len(token) > 256 {
		return Node{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var id string
	var stored []byte
	err := s.db.QueryRowContext(ctx, "SELECT id, token_hash FROM nodes WHERE token_hash = ?", digest[:]).Scan(&id, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrUnauthenticated
	}
	if err != nil {
		return Node{}, err
	}
	if subtle.ConstantTimeCompare(stored, digest[:]) != 1 {
		return Node{}, ErrUnauthenticated
	}
	return s.Get(ctx, id)
}

// Delete revokes identity and cancels pending work in the same transaction as
// asset deletion. Update results retain the deleted node ID for auditability.
func (s *Service) Delete(ctx context.Context, id string, confirm bool) error {
	if !confirm {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET token_hash = NULL, installation_id = NULL WHERE id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE update_job_nodes SET status = ? WHERE node_id = ? AND status IN ('queued','downloading','verifying','restarting','awaiting_report')", "cancelled", id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM nodes WHERE id = ?", id)
	if err := affected(result, err); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "node.delete", id, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Batch(ctx context.Context, req BatchRequest) ([]BatchResult, error) {
	if len(req.NodeIDs) == 0 || len(req.NodeIDs) > 500 {
		return nil, ErrInvalidInput
	}
	if req.Action != "disable" && req.Action != "enable" && req.Action != "delete" {
		return nil, ErrInvalidInput
	}
	if req.Action == "delete" && !req.Confirm {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]bool, len(req.NodeIDs))
	for _, id := range req.NodeIDs {
		if !identity.ValidUUID(id) || seen[id] {
			return nil, ErrInvalidInput
		}
		seen[id] = true
	}
	results := make([]BatchResult, 0, len(req.NodeIDs))
	for _, id := range req.NodeIDs {
		var err error
		if req.Action == "delete" {
			err = s.Delete(ctx, id, req.Confirm)
		} else {
			err = s.SetDisabled(ctx, id, req.Action == "disable")
		}
		result := BatchResult{NodeID: id, Status: "succeeded"}
		if err != nil {
			result.Status = "failed"
			if errors.Is(err, ErrNotFound) {
				result.ErrorCode = "node_not_found"
			} else {
				result.ErrorCode = "storage_error"
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func readConfig(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (DesiredConfig, error) {
	var cfg DesiredConfig
	var interfaces, disks, probes string
	err := query.QueryRowContext(ctx, `SELECT desired_revision, report_interval_seconds, disk_interval_seconds, interfaces, disk_paths, probe_targets, billing_day, quota_bytes FROM nodes WHERE id = ?`, id).Scan(&cfg.Revision, &cfg.ReportIntervalSeconds, &cfg.DiskIntervalSeconds, &interfaces, &disks, &probes, &cfg.BillingDay, &cfg.QuotaBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return DesiredConfig{}, ErrNotFound
	}
	if err != nil {
		return DesiredConfig{}, err
	}
	if err := json.Unmarshal([]byte(interfaces), &cfg.Interfaces); err != nil {
		return DesiredConfig{}, err
	}
	if err := json.Unmarshal([]byte(disks), &cfg.DiskPaths); err != nil {
		return DesiredConfig{}, err
	}
	if err := json.Unmarshal([]byte(probes), &cfg.ProbeTargets); err != nil {
		return DesiredConfig{}, err
	}
	return cfg, nil
}

func (s *Service) GetConfig(ctx context.Context, id string) (DesiredConfig, error) {
	return readConfig(ctx, s.db, id)
}

func (s *Service) PatchConfig(ctx context.Context, id string, patch ConfigPatch) (DesiredConfig, error) {
	fields := make(map[string]string)
	if p := patch.ReportIntervalSeconds; p != nil && (*p < 5 || *p > 60) {
		fields["report_interval_seconds"] = "must be between 5 and 60"
	}
	if p := patch.DiskIntervalSeconds; p != nil && (*p < 60 || *p > 3600) {
		fields["disk_interval_seconds"] = "must be between 60 and 3600"
	}
	if p := patch.BillingDay; p != nil && (*p < 1 || *p > 28) {
		fields["billing_day"] = "must be between 1 and 28"
	}
	if p := patch.QuotaBytes; p != nil {
		if err := v1.DecimalUint64(*p).Validate(); err != nil {
			fields["quota_bytes"] = "must be a canonical unsigned decimal byte count"
		}
	}
	var selection v1.DesiredConfig
	if patch.Interfaces != nil {
		selection.Interfaces = *patch.Interfaces
	}
	if patch.DiskPaths != nil {
		selection.DiskPaths = *patch.DiskPaths
	}
	if err := selection.ValidateSelection(); err != nil {
		for field, message := range err.(*v1.ValidationError).Fields {
			fields[field] = message
		}
	}
	if len(fields) != 0 {
		return DesiredConfig{}, &ValidationError{Fields: fields}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DesiredConfig{}, err
	}
	defer tx.Rollback()
	cfg, err := readConfig(ctx, tx, id)
	if err != nil {
		return DesiredConfig{}, err
	}
	if patch.ReportIntervalSeconds != nil {
		cfg.ReportIntervalSeconds = *patch.ReportIntervalSeconds
	}
	if patch.DiskIntervalSeconds != nil {
		cfg.DiskIntervalSeconds = *patch.DiskIntervalSeconds
	}
	if patch.BillingDay != nil {
		cfg.BillingDay = *patch.BillingDay
	}
	if patch.Interfaces != nil {
		cfg.Interfaces = *patch.Interfaces
	}
	if patch.DiskPaths != nil {
		cfg.DiskPaths = *patch.DiskPaths
	}
	if patch.QuotaBytes != nil {
		cfg.QuotaBytes = *patch.QuotaBytes
	}
	// A partial PATCH must also fit together with the persisted other selection.
	if err := (v1.DesiredConfig{Interfaces: cfg.Interfaces, DiskPaths: cfg.DiskPaths}).ValidateSelection(); err != nil {
		return DesiredConfig{}, &ValidationError{Fields: err.(*v1.ValidationError).Fields}
	}
	interfaces, err := json.Marshal(cfg.Interfaces)
	if err != nil {
		return DesiredConfig{}, err
	}
	disks, err := json.Marshal(cfg.DiskPaths)
	if err != nil {
		return DesiredConfig{}, err
	}
	cfg.Revision++
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET desired_revision = ?, report_interval_seconds = ?, disk_interval_seconds = ?, interfaces = ?, disk_paths = ?, billing_day = ?, quota_bytes = ? WHERE id = ?`, cfg.Revision, cfg.ReportIntervalSeconds, cfg.DiskIntervalSeconds, string(interfaces), string(disks), cfg.BillingDay, cfg.QuotaBytes, id); err != nil {
		return DesiredConfig{}, err
	}
	if err := audit.Insert(ctx, tx, "node.config.patch", id, s.now()); err != nil {
		return DesiredConfig{}, err
	}
	if err := tx.Commit(); err != nil {
		return DesiredConfig{}, err
	}
	return cfg, nil
}
