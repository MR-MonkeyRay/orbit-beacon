package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

var (
	ErrProtocolUnsupported = errors.New("protocol version unsupported")
	ErrInvalidReport       = errors.New("invalid report")
	ErrIdentityConflict    = errors.New("report process identity conflict")
	ErrRateLimited         = errors.New("report rate limited")
)

type Options struct {
	Now func() time.Time
}

type nodeState struct {
	credential [sha256.Size]byte
	generation uint64
	session    string
	seq        uint64
	latest     []byte
	tokens     float64
	refilled   time.Time
}

type Service struct {
	db     *sql.DB
	now    func() time.Time
	mu     sync.Mutex
	states map[string]*nodeState
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{db: db, now: opts.Now, states: make(map[string]*nodeState)}
}

type Admission struct {
	service    *Service
	nodeID     string
	credential [sha256.Size]byte
	used       bool
}

func (s *Service) AdmitToken(ctx context.Context, token string) (*Admission, error) {
	if len(token) < 32 || len(token) > 256 {
		return nil, nodes.ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var nodeID string
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM nodes WHERE token_hash = ?", digest[:]).Scan(&nodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nodes.ErrUnauthenticated
		}
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.charge(ctx, nodeID, s.db); err != nil {
		return nil, err
	}
	return &Admission{service: s, nodeID: nodeID, credential: digest}, nil
}

func (s *Service) AcceptAdmitted(ctx context.Context, admission *Admission, report v1.Report) (v1.ReportResponse, error) {
	s.mu.Lock()
	if admission == nil || admission.service != s || admission.used {
		s.mu.Unlock()
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	admission.used = true
	s.mu.Unlock()
	return s.accept(ctx, admission.nodeID, admission.credential[:], report, false)
}

func (s *Service) AcceptToken(ctx context.Context, token string, report v1.Report) (v1.ReportResponse, error) {
	admission, err := s.AdmitToken(ctx, token)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	return s.AcceptAdmitted(ctx, admission, report)
}

func (s *Service) charge(ctx context.Context, nodeID string, query rowQueryer) error {
	now := s.now().UTC()
	state := s.states[nodeID]
	if state == nil {
		if err := s.prune(ctx, query); err != nil {
			return err
		}
		state = &nodeState{tokens: 3, refilled: now}
		s.states[nodeID] = state
	}
	elapsed := now.Sub(state.refilled).Seconds()
	if elapsed > 0 {
		state.tokens = min(3, state.tokens+elapsed)
		state.refilled = now
	}
	if state.tokens < 1 {
		return ErrRateLimited
	}
	state.tokens--
	return nil
}

// Accept fences process identity durably; sequence deduplication and snapshots
// intentionally last only for this server process.
func (s *Service) Accept(ctx context.Context, nodeID string, report v1.Report) (v1.ReportResponse, error) {
	return s.accept(ctx, nodeID, nil, report, true)
}

func (s *Service) accept(ctx context.Context, nodeID string, credential []byte, report v1.Report, charge bool) (v1.ReportResponse, error) {
	if report.ProtocolVersion != 1 {
		return v1.ReportResponse{}, ErrProtocolUnsupported
	}
	if err := report.Validate(); err != nil {
		return v1.ReportResponse{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer tx.Rollback()
	var generationText, session string
	var storedCredential []byte
	if err := tx.QueryRowContext(ctx, "SELECT run_generation, report_session_id, token_hash FROM nodes WHERE id = ?", nodeID).Scan(&generationText, &session, &storedCredential); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v1.ReportResponse{}, nodes.ErrUnauthenticated
		}
		return v1.ReportResponse{}, err
	}
	// Recheck in the write transaction so rotation cannot race authentication.
	if len(storedCredential) == 0 || (credential != nil && subtle.ConstantTimeCompare(storedCredential, credential) != 1) {
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	if report.RunGeneration < generation || (report.RunGeneration == generation && session != "" && session != report.SessionID) {
		return v1.ReportResponse{}, ErrIdentityConflict
	}
	if charge {
		if err := s.charge(ctx, nodeID, tx); err != nil {
			return v1.ReportResponse{}, err
		}
	}
	state := s.states[nodeID]
	response := v1.ReportResponse{NodeID: nodeID, AcceptedSeq: report.Seq, ServerTime: now}
	desired, err := readConfig(ctx, tx, nodeID)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	if desired.Revision > report.ConfigRevision {
		response.DesiredConfig = &desired
	}
	sameCredential := subtle.ConstantTimeCompare(state.credential[:], storedCredential) == 1
	if sameCredential && state.generation == report.RunGeneration && state.session == report.SessionID && report.Seq <= state.seq {
		response.AcceptedSeq = state.seq
		return response, nil
	}
	body, err := json.Marshal(report)
	if err != nil {
		return v1.ReportResponse{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	configError := ""
	if report.ConfigError != nil {
		encoded, err := json.Marshal(report.ConfigError)
		if err != nil {
			return v1.ReportResponse{}, err
		}
		configError = string(encoded)
	}
	var system any
	if report.System != nil {
		encoded, err := json.Marshal(report.System)
		if err != nil {
			return v1.ReportResponse{}, err
		}
		system = string(encoded)
	}
	// Rejection cannot erase a prior successful configuration acknowledgement.
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET run_generation = ?, report_session_id = ?, last_seen = ?, system = COALESCE(?, system), applied_revision = CASE WHEN ? = '' AND ? <= desired_revision THEN max(applied_revision, ?) ELSE applied_revision END, config_error = ? WHERE id = ?`, strconv.FormatUint(report.RunGeneration, 10), report.SessionID, now.Format(time.RFC3339Nano), system, configError, report.ConfigRevision, report.ConfigRevision, configError, nodeID); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return v1.ReportResponse{}, err
	}
	state.generation = report.RunGeneration
	copy(state.credential[:], storedCredential)
	state.session = report.SessionID
	state.seq = report.Seq
	state.latest = body
	return response, nil
}

type rowQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Service) prune(ctx context.Context, query rowQueryer) error {
	rows, err := query.QueryContext(ctx, "SELECT id FROM nodes")
	if err != nil {
		return err
	}
	defer rows.Close()
	live := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		live[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id := range s.states {
		if !live[id] {
			delete(s.states, id)
		}
	}
	return nil
}

func readConfig(ctx context.Context, tx *sql.Tx, nodeID string) (v1.DesiredConfig, error) {
	var desired v1.DesiredConfig
	var interfaces, disks, probes string
	if err := tx.QueryRowContext(ctx, `SELECT desired_revision, report_interval_seconds, disk_interval_seconds, interfaces, disk_paths, probe_targets, billing_day, quota_bytes FROM nodes WHERE id = ?`, nodeID).Scan(&desired.Revision, &desired.ReportIntervalSeconds, &desired.DiskIntervalSeconds, &interfaces, &disks, &probes, &desired.BillingDay, &desired.QuotaBytes); err != nil {
		return v1.DesiredConfig{}, err
	}
	if err := json.Unmarshal([]byte(interfaces), &desired.Interfaces); err != nil {
		return v1.DesiredConfig{}, err
	}
	if err := json.Unmarshal([]byte(disks), &desired.DiskPaths); err != nil {
		return v1.DesiredConfig{}, err
	}
	if err := json.Unmarshal([]byte(probes), &desired.ProbeTargets); err != nil {
		return v1.DesiredConfig{}, err
	}
	return desired, nil
}

func (s *Service) Latest(ctx context.Context, nodeID string) (v1.Report, error) {
	if err := ctx.Err(); err != nil {
		return v1.Report{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[nodeID]
	if state == nil || state.latest == nil {
		return v1.Report{}, nodes.ErrNotFound
	}
	var report v1.Report
	if err := json.Unmarshal(state.latest, &report); err != nil {
		return v1.Report{}, err
	}
	return report, nil
}
