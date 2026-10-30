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
	"sync/atomic"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/change"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

type serviceError string

func (err serviceError) Error() string { return string(err) }

const (
	ErrCapacityReached serviceError = "node capacity reached"
	ErrNotFound        serviceError = "node not found"
	ErrUnauthenticated serviceError = "node authentication required"
	ErrInvalidInput    serviceError = "invalid node request"
	// ErrDisabled rejects agent traffic for a disabled node without treating
	// its still-valid credential as unknown.
	ErrDisabled serviceError = "node is disabled"
)

type Node struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	Notes        string     `json:"notes"`
	Region       string     `json:"region"`
	Provider     string     `json:"provider"`
	GroupID      *string    `json:"group_id"`
	Tags         []string   `json:"tags"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSeen     *time.Time `json:"last_seen"`
	Presence     string     `json:"presence"`
	AgentVersion string     `json:"agent_version"`
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

type TrafficSettings struct {
	BillingDay int    `json:"billing_day"`
	QuotaBytes string `json:"quota_bytes"`
}

type TrafficSettingsPatch struct {
	BillingDay    *int    `json:"billing_day,omitempty"`
	QuotaBytes    *string `json:"quota_bytes,omitempty"`
	invalidFields map[string]string
}

type Options struct {
	MaxNodes int
	Now      func() time.Time
	Random   io.Reader
}

type Service struct {
	publicMu            sync.RWMutex
	publicVersion       uint64
	publicSafetyChanges change.Notifier
	publicReader        atomic.Pointer[sql.DB]
	db                  *sql.DB
	maxNodes            int
	now                 func() time.Time
	random              io.Reader
	randomMu            sync.Mutex
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

// InvalidatePublic expires projections after an ordinary committed data change.
// Additions and text edits stay on the browser's configured update cadence.
func (s *Service) InvalidatePublic() {
	s.publicMu.Lock()
	s.publicVersion++
	s.publicMu.Unlock()
}

// InvalidatePublicSafety expires data and immediately renews active streams.
// Publish both versions under one lock so cache and egress cannot disagree.
func (s *Service) InvalidatePublicSafety() {
	s.publicMu.Lock()
	s.publicVersion++
	s.publicSafetyChanges.Notify()
	s.publicMu.Unlock()
}

func (s *Service) PublicSafetyChanges() (uint64, <-chan struct{}) {
	s.publicMu.RLock()
	defer s.publicMu.RUnlock()
	return s.publicSafetyChanges.Snapshot()
}

func (s *Service) PublicSafetyVersion() uint64 {
	s.publicMu.RLock()
	defer s.publicMu.RUnlock()
	version, _ := s.publicSafetyChanges.Snapshot()
	return version
}

func (s *Service) CreatePending(ctx context.Context, req CreateRequest) (Node, error) {
	if strings.TrimSpace(req.Name) == "" || len(req.Name) > 255 {
		return Node{}, ErrInvalidInput
	}
	s.randomMu.Lock()
	id, err := uuid.UUID(s.random)
	s.randomMu.Unlock()
	if err != nil {
		return Node{}, err
	}
	node := Node{ID: id, Name: req.Name, Status: "pending", Presence: "pending", Tags: []string{}, CreatedAt: s.now().UTC()}
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
	if _, err := tx.ExecContext(ctx, "INSERT INTO nodes(id, name, status, created_at) VALUES (?, ?, ?, ?)", id, req.Name, node.Status, timekey.Format(node.CreatedAt)); err != nil {
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
	(SELECT json_group_array(tag) FROM (SELECT tag FROM node_tags WHERE node_id = nodes.id ORDER BY tag)), last_seen, length(token_hash) > 0, system`

func scanNode(row scanner, now time.Time) (Node, error) {
	var node Node
	var created, tags, systemJSON string
	var lastSeen sql.NullString
	var hasCredential sql.NullBool
	if err := row.Scan(&node.ID, &node.Name, &node.Status, &created, &node.Notes, &node.Region, &node.Provider, &node.GroupID, &tags, &lastSeen, &hasCredential, &systemJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	var err error
	node.CreatedAt, err = timekey.Parse(created)
	if err != nil {
		return Node{}, err
	}
	if err := json.Unmarshal([]byte(tags), &node.Tags); err != nil {
		return Node{}, err
	}
	if node.Tags == nil {
		node.Tags = []string{}
	}
	if lastSeen.Valid {
		seen, err := timekey.Parse(lastSeen.String)
		if err != nil {
			return Node{}, err
		}
		node.LastSeen = &seen
	}
	// HTTP inventory uses durable observations; live reads overlay only a
	// strictly matching in-memory process identity.
	node.Presence = Presence(node.Status, hasCredential.Bool, node.LastSeen, now)
	var system v1.System
	if json.Unmarshal([]byte(systemJSON), &system) == nil {
		node.AgentVersion = system.AgentVersion
	}
	return node, nil
}

func (s *Service) Get(ctx context.Context, id string) (Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = ?", id), s.now())
}

// ListPage fetches one lookahead row in the same query as the requested page.
func (s *Service) ListPage(ctx context.Context, limit, offset int) ([]Node, bool, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, false, ErrInvalidInput
	}
	result, err := s.list(ctx, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore, nil
}

func (s *Service) list(ctx context.Context, limit, offset int) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+nodeColumns+" FROM nodes ORDER BY created_at, id LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Node, 0)
	now := s.now()
	for rows.Next() {
		node, err := scanNode(rows, now)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

// PatchMetadata changes only supplied metadata and replaces tags only when present.
func (s *Service) PatchMetadata(ctx context.Context, id string, patch MetadataPatch) (Node, error) {
	if !uuid.ValidUUID(id) {
		return Node{}, ErrInvalidInput
	}
	fields := make(map[string]string)
	if patch.Name != nil && (strings.TrimSpace(*patch.Name) == "" || len(*patch.Name) > 255) {
		fields["name"] = "must be nonempty and at most 255 bytes"
	}
	var groupID *string
	if patch.GroupID != nil {
		if err := json.Unmarshal(patch.GroupID, &groupID); err != nil || (groupID != nil && !uuid.ValidUUID(*groupID)) {
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
		return Node{}, &validation.Error{Fields: fields}
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
			return Node{}, validation.Invalid("group_id", "group does not exist")
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
	node, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = ?", id), s.now())
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
	if err := tx.Commit(); err != nil {
		return err
	}
	if disabled {
		s.InvalidatePublicSafety()
	} else {
		s.InvalidatePublic()
	}
	return nil
}

// RevokeCredential detaches the agent identity but keeps the asset, so a leaked
// credential can be cut off without losing metadata or public settings. A new
// single-node enrollment ticket can bind a fresh installation afterwards.
func (s *Service) RevokeCredential(ctx context.Context, id string) error {
	if !uuid.ValidUUID(id) {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Exhaustion must roll back, not let SQLite promote the identity fence to
	// REAL. The existing NOT NULL constraint rejects only an actual increment.
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET credential_epoch = CASE WHEN length(token_hash)>0 THEN CASE WHEN credential_epoch < 9223372036854775807 THEN credential_epoch + 1 ELSE NULL END ELSE credential_epoch END, token_hash = NULL, installation_id = NULL, run_generation = '0', report_session_id = '', agent_runtime = '', last_seen = NULL, system = '{}', status = CASE WHEN status = 'disabled' THEN 'disabled' ELSE 'pending' END WHERE id = ?`, id)
	if err := affected(result, err); err != nil {
		return err
	}
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE node_id=?", id); err != nil {
			return err
		}
	}
	if err := audit.Insert(ctx, tx, "node.credential.revoke", id, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidatePublicSafety()
	return nil
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

// Authentication is the internal credential identity observed by one read.
// It is not a node DTO and must not be exposed over the API.
type Authentication struct {
	ID              string `json:"-"`
	CredentialEpoch int64  `json:"-"`
}

func (s *Service) AuthenticateToken(ctx context.Context, token string) (Authentication, error) {
	if len(token) < 32 || len(token) > 256 {
		return Authentication{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var identity Authentication
	var status string
	var stored []byte
	err := s.db.QueryRowContext(ctx, "SELECT id, credential_epoch, token_hash, status FROM nodes WHERE token_hash = ?", digest[:]).Scan(&identity.ID, &identity.CredentialEpoch, &stored, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Authentication{}, ErrUnauthenticated
	}
	if err != nil {
		return Authentication{}, err
	}
	if subtle.ConstantTimeCompare(stored, digest[:]) != 1 {
		return Authentication{}, ErrUnauthenticated
	}
	if status == "disabled" {
		return Authentication{}, ErrDisabled
	}
	return identity, nil
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
	if err := audit.ScopedAlertRuleDeletes(ctx, tx, "node", id, s.now()); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM nodes WHERE id = ?", id)
	if err := affected(result, err); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "node.delete", id, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidatePublicSafety()
	return nil
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
		if !uuid.ValidUUID(id) || seen[id] {
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
			} else if storage.IsBusy(err) {
				result.ErrorCode = "storage_busy"
			} else {
				result.ErrorCode = "storage_error"
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// GetRuntime returns the current identity's last observed agent settings.
func (s *Service) GetRuntime(ctx context.Context, id string) (*v1.RuntimeSettings, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT CASE WHEN length(token_hash)>0 AND run_generation!='0' AND report_session_id!='' THEN agent_runtime ELSE '' END FROM nodes WHERE id=?", id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	runtime, err := v1.DecodeRuntimeSettings(raw)
	if err != nil {
		return nil, err
	}
	return &runtime, nil
}

func readTrafficSettings(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (TrafficSettings, error) {
	var settings TrafficSettings
	err := q.QueryRowContext(ctx, "SELECT billing_day, quota_bytes FROM nodes WHERE id=?", id).Scan(&settings.BillingDay, &settings.QuotaBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return TrafficSettings{}, ErrNotFound
	}
	return settings, err
}

func (s *Service) GetTrafficSettings(ctx context.Context, id string) (TrafficSettings, error) {
	return readTrafficSettings(ctx, s.db, id)
}

func (s *Service) PatchTrafficSettings(ctx context.Context, id string, patch TrafficSettingsPatch) (TrafficSettings, error) {
	fields := make(map[string]string)
	for field, reason := range patch.invalidFields {
		fields[field] = reason
	}
	if p := patch.BillingDay; p != nil && (*p < 1 || *p > 28) {
		fields["billing_day"] = "must be between 1 and 28"
	}
	if p := patch.QuotaBytes; p != nil {
		if err := v1.DecimalUint64(*p).Validate(); err != nil {
			fields["quota_bytes"] = "must be a canonical unsigned decimal byte count"
		}
	}
	if len(fields) != 0 {
		return TrafficSettings{}, &validation.Error{Fields: fields}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TrafficSettings{}, err
	}
	defer tx.Rollback()
	settings, err := readTrafficSettings(ctx, tx, id)
	if err != nil {
		return TrafficSettings{}, err
	}
	before := settings
	if patch.BillingDay != nil {
		settings.BillingDay = *patch.BillingDay
	}
	if patch.QuotaBytes != nil {
		settings.QuotaBytes = *patch.QuotaBytes
	}
	if settings == before {
		return settings, nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET billing_day=?, quota_bytes=? WHERE id=?", settings.BillingDay, settings.QuotaBytes, id); err != nil {
		return TrafficSettings{}, err
	}
	if err := audit.Insert(ctx, tx, "node.traffic_settings.patch", id, s.now()); err != nil {
		return TrafficSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return TrafficSettings{}, err
	}
	return settings, nil
}
