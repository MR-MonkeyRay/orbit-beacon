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
	"orbitbeacon/internal/timekey"
)

var (
	ErrProtocolUnsupported = errors.New("protocol version unsupported")
	ErrInvalidReport       = errors.New("invalid report")
	ErrIdentityConflict    = errors.New("report process identity conflict")
	ErrRateLimited         = errors.New("report rate limited")
	ErrWriterBusy          = errors.New("report writer queue is full")
	ErrServiceClosed       = errors.New("report ingestion service is closed")
)

const writerQueueCapacity = 8

type Options struct {
	Now func() time.Time
}

type nodeState struct {
	mu         sync.Mutex
	refs       int
	touch      uint64
	credential [sha256.Size]byte
	generation uint64
	session    string
	seq        uint64
	latest     []byte
	public     publicMetrics
	tokens     float64
	refilled   time.Time
}

type Service struct {
	db          *sql.DB
	now         func() time.Time
	mu          sync.Mutex
	rateMu      sync.Mutex
	epoch       uint64
	states      map[string]*nodeState
	writerQueue chan struct{}
	writer      chan struct{}
	lifecycleMu sync.Mutex
	inflight    int
	closing     bool
	done        chan struct{}
	doneClosed  bool
}

type stateCandidate struct {
	state *nodeState
	touch uint64
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{db: db, now: opts.Now, states: make(map[string]*nodeState), writerQueue: make(chan struct{}, writerQueueCapacity), writer: make(chan struct{}, 1), done: make(chan struct{})}
}

func (s *Service) beginWrite(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.lifecycleMu.Lock()
	if s.closing {
		s.lifecycleMu.Unlock()
		return nil, ErrServiceClosed
	}
	s.inflight++
	s.lifecycleMu.Unlock()
	finish := func() {
		s.lifecycleMu.Lock()
		s.inflight--
		if s.closing && s.inflight == 0 && !s.doneClosed {
			close(s.done)
			s.doneClosed = true
		}
		s.lifecycleMu.Unlock()
	}
	select {
	case s.writerQueue <- struct{}{}:
	case <-ctx.Done():
		finish()
		return nil, ctx.Err()
	default:
		finish()
		return nil, ErrWriterBusy
	}
	if err := ctx.Err(); err != nil {
		<-s.writerQueue
		finish()
		return nil, err
	}
	select {
	case s.writer <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.writer
			<-s.writerQueue
			finish()
			return nil, err
		}
		return func() {
			<-s.writer
			<-s.writerQueue
			finish()
		}, nil
	case <-ctx.Done():
		<-s.writerQueue
		finish()
		return nil, ctx.Err()
	}
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.closing = true
	if s.inflight == 0 && !s.doneClosed {
		close(s.done)
		s.doneClosed = true
	}
	done := s.done
	s.lifecycleMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type Admission struct {
	service    *Service
	nodeID     string
	credential [sha256.Size]byte
	usedMu     sync.Mutex
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
	_, exists := s.states[nodeID]
	s.mu.Unlock()
	if !exists {
		if err := s.prune(ctx, s.db); err != nil {
			return nil, err
		}
	}
	state := s.acquireState(nodeID)
	defer s.releaseState(state)
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if err := s.charge(state); err != nil {
		return nil, err
	}
	return &Admission{service: s, nodeID: nodeID, credential: digest}, nil
}

func (s *Service) AcceptAdmitted(ctx context.Context, admission *Admission, report v1.Report) (v1.ReportResponse, error) {
	if admission == nil || admission.service != s {
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	admission.usedMu.Lock()
	if admission.used {
		admission.usedMu.Unlock()
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	admission.used = true
	admission.usedMu.Unlock()
	return s.accept(ctx, admission.nodeID, admission.credential[:], report, false)
}

func (s *Service) AcceptToken(ctx context.Context, token string, report v1.Report) (v1.ReportResponse, error) {
	admission, err := s.AdmitToken(ctx, token)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	return s.AcceptAdmitted(ctx, admission, report)
}

func (s *Service) acquireState(nodeID string) *nodeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[nodeID]
	if state == nil {
		state = &nodeState{}
		s.states[nodeID] = state
	}
	s.epoch++
	state.touch = s.epoch
	state.refs++
	return state
}

func (s *Service) acquireExistingState(nodeID string) *nodeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[nodeID]
	if state == nil {
		return nil
	}
	s.epoch++
	state.touch = s.epoch
	state.refs++
	return state
}

func (s *Service) releaseState(state *nodeState) {
	s.mu.Lock()
	state.refs--
	s.mu.Unlock()
}

// charge must be called with rateMu held.
func (s *Service) charge(state *nodeState) error {
	now := s.now().UTC()
	if state.refilled.IsZero() {
		state.tokens = 3
		state.refilled = now
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
	releaseWrite, err := s.beginWrite(ctx)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer releaseWrite()
	state := s.acquireState(nodeID)
	defer s.releaseState(state)
	state.mu.Lock()
	defer state.mu.Unlock()
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
		s.rateMu.Lock()
		err := s.charge(state)
		s.rateMu.Unlock()
		if err != nil {
			return v1.ReportResponse{}, err
		}
	}
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
	// Rejection cannot erase a prior successful configuration acknowledgement.
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET run_generation = ?, report_session_id = ?, last_seen = ?, system = COALESCE(?, system), applied_revision = CASE WHEN ? = '' AND ? <= desired_revision THEN max(applied_revision, ?) ELSE applied_revision END, config_error = ? WHERE id = ?`, strconv.FormatUint(report.RunGeneration, 10), report.SessionID, timekey.Format(now), system, configError, report.ConfigRevision, report.ConfigRevision, configError, nodeID); err != nil {
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
	state.public = snapshotPublic(report.Metrics, now)
	return response, nil
}

type rowQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Service) prune(ctx context.Context, query rowQueryer) error {
	candidates := s.stateCandidates()
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
	s.pruneStates(live, candidates)
	return nil
}

func (s *Service) stateCandidates() map[string]stateCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := make(map[string]stateCandidate)
	for id, state := range s.states {
		if state.refs == 0 {
			candidates[id] = stateCandidate{state: state, touch: state.touch}
		}
	}
	return candidates
}

func (s *Service) pruneStates(live map[string]bool, candidates map[string]stateCandidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, candidate := range candidates {
		state := s.states[id]
		if live[id] || state != candidate.state || state.refs != 0 || state.touch != candidate.touch {
			continue
		}
		delete(s.states, id)
	}
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
	state := s.acquireExistingState(nodeID)
	if state == nil {
		return v1.Report{}, nodes.ErrNotFound
	}
	state.mu.Lock()
	body := append([]byte(nil), state.latest...)
	state.mu.Unlock()
	s.releaseState(state)
	if body == nil {
		return v1.Report{}, nodes.ErrNotFound
	}
	var report v1.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return v1.Report{}, err
	}
	return report, nil
}
