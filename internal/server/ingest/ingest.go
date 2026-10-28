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
	"sync/atomic"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/change"
	"orbitbeacon/internal/server/history"
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
	// Samples receives owned accepted snapshots via non-blocking sends. A full
	// buffer drops history without delaying report acceptance.
	Samples chan<- history.Sample
	Now     func() time.Time
	// PersistInterval bounds durable last_seen lag; nonpositive means 30 seconds.
	PersistInterval time.Duration
}

type nodeState struct {
	mu                sync.Mutex
	acceptGate        chan struct{}
	refs              int
	touch             uint64
	credential        [sha256.Size]byte
	generation        uint64
	session           string
	seq               uint64
	latest            []byte
	receivedAt        time.Time
	lastSeenPersisted time.Time
	probes            probeBuffer
	traffic           trafficBuffer
	stagedTraffic     *trafficBuffer
	public            publicMetrics
	tokens            float64
	refilled          time.Time
}

// These helpers require state.mu to be held. Rate history is independent
// of process identity and survives invalidation.
func (state *nodeState) matchesIdentity(credential []byte, generation uint64, session string) bool {
	return len(state.latest) != 0 && subtle.ConstantTimeCompare(state.credential[:], credential) == 1 && state.generation == generation && state.session == session
}

func (state *nodeState) invalidateIdentity(credential []byte, generation uint64, session string) {
	state.fenceTraffic(credential, generation, session)
	state.fenceProbes(credential, generation, session)
	if state.matchesIdentity(credential, generation, session) {
		return
	}
	state.credential = [sha256.Size]byte{}
	state.generation, state.session, state.seq = 0, "", 0
	state.latest = nil
	state.receivedAt, state.lastSeenPersisted = time.Time{}, time.Time{}
	state.public = publicMetrics{}
}

type Service struct {
	samples         chan<- history.Sample
	samplesDropped  atomic.Uint64
	publicChanges   change.Notifier
	publicPages     publicPageCache
	db              *sql.DB
	now             func() time.Time
	persistInterval time.Duration
	mu              sync.Mutex
	rateMu          sync.Mutex
	epoch           uint64
	states          map[string]*nodeState
	writerQueue     chan struct{}
	writer          chan struct{}
	lifecycleMu     sync.Mutex
	inflight        int
	closing         bool
	done            chan struct{}
	doneClosed      bool
}

type stateCandidate struct {
	state *nodeState
	touch uint64
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PersistInterval <= 0 {
		opts.PersistInterval = 30 * time.Second
	}
	return &Service{samples: opts.Samples, db: db, now: opts.Now, persistInterval: opts.PersistInterval, states: make(map[string]*nodeState), writerQueue: make(chan struct{}, writerQueueCapacity), writer: make(chan struct{}, 1), done: make(chan struct{})}
}

func (s *Service) PublicChanges() (uint64, <-chan struct{}) {
	return s.publicChanges.Snapshot()
}

func (s *Service) beginWrite(ctx context.Context) (func(), error) {
	finish, err := s.beginUse(ctx)
	if err != nil {
		return nil, err
	}
	return s.queueWrite(ctx, finish)
}

// beginUse covers in-memory acceptance too, so Shutdown drains both paths.
func (s *Service) beginUse(ctx context.Context) (func(), error) {
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
	return func() {
		s.lifecycleMu.Lock()
		s.inflight--
		if s.closing && s.inflight == 0 && !s.doneClosed {
			close(s.done)
			s.doneClosed = true
		}
		s.lifecycleMu.Unlock()
	}, nil
}

func (s *Service) queueWrite(ctx context.Context, finish func()) (func(), error) {
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
		return s.drainTraffic(ctx)
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
	var nodeID, status string
	if err := s.db.QueryRowContext(ctx, "SELECT id, status FROM nodes WHERE token_hash = ?", digest[:]).Scan(&nodeID, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nodes.ErrUnauthenticated
		}
		return nil, err
	}
	// Disabling stops ingestion before any request body is read or rate charged.
	if status == "disabled" {
		return nil, nodes.ErrDisabled
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
		state = &nodeState{acceptGate: make(chan struct{}, 1)}
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
	finish, err := s.beginUse(ctx)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer finish()
	if report.ProtocolVersion != 1 {
		return v1.ReportResponse{}, ErrProtocolUnsupported
	}
	if err := report.Validate(); err != nil {
		return v1.ReportResponse{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	if err := validateTraffic(report.Traffic); err != nil {
		return v1.ReportResponse{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	// This server never issues revisions above the ceiling, so a larger value
	// cannot be a real acknowledgement and would starve later configuration.
	if report.ConfigRevision > nodes.MaxConfigRevision {
		return v1.ReportResponse{}, fmt.Errorf("%w: config revision exceeds the server limit", ErrInvalidReport)
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
	var systemHash [sha256.Size]byte
	if report.System != nil {
		encoded, err := json.Marshal(report.System)
		if err != nil {
			return v1.ReportResponse{}, err
		}
		system = string(encoded)
		systemHash = sha256.Sum256(encoded)
	}
	state := s.acquireState(nodeID)
	defer s.releaseState(state)
	// Serialize reports without making a cancelled request wait for a slow
	// same-node writer. mu protects only the short snapshot copies below.
	select {
	case state.acceptGate <- struct{}{}:
		defer func() { <-state.acceptGate }()
	case <-ctx.Done():
		return v1.ReportResponse{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return v1.ReportResponse{}, err
	}
	// The read-only authentication fence is linearized at the SELECT below.
	// A credential/status change committed before it is always observed; a
	// change committed after it takes effect after this concurrent request.
	var generationText, session, status, storedSystem, storedConfigError string
	var lastSeenText sql.NullString
	var storedCredential []byte
	var appliedRevision, desiredRevision int64
	if err := s.db.QueryRowContext(ctx, "SELECT run_generation, report_session_id, token_hash, status, applied_revision, desired_revision, COALESCE(system, ''), config_error, last_seen FROM nodes WHERE id = ?", nodeID).Scan(&generationText, &session, &storedCredential, &status, &appliedRevision, &desiredRevision, &storedSystem, &storedConfigError, &lastSeenText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v1.ReportResponse{}, nodes.ErrUnauthenticated
		}
		return v1.ReportResponse{}, err
	}
	if len(storedCredential) == 0 || (credential != nil && subtle.ConstantTimeCompare(storedCredential, credential) != 1) {
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	if status == "disabled" {
		return v1.ReportResponse{}, nodes.ErrDisabled
	}
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	state.mu.Lock()
	state.invalidateIdentity(storedCredential, generation, session)
	cachedCredential, cachedGeneration, cachedSession := state.credential, state.generation, state.session
	cachedSeq, hasLatest := state.seq, len(state.latest) != 0
	state.mu.Unlock()
	now := s.now().UTC()
	if report.RunGeneration < generation || (report.RunGeneration == generation && session != "" && session != report.SessionID) {
		return v1.ReportResponse{}, ErrIdentityConflict
	}
	desired := v1.DesiredConfig{Revision: desiredRevision}
	var persistedLastSeen time.Time
	if lastSeenText.Valid {
		persistedLastSeen, err = timekey.Parse(lastSeenText.String)
		if err != nil {
			return v1.ReportResponse{}, err
		}
	}
	// Dedupe belongs to the durable process fence as well as the credential:
	// revoke/re-enroll may reuse a credential while resetting that fence (ABA).
	sameState := hasLatest && subtle.ConstantTimeCompare(cachedCredential[:], storedCredential) == 1 &&
		cachedGeneration == generation && cachedSession == session &&
		cachedGeneration == report.RunGeneration && cachedSession == report.SessionID
	storedSystemHash := sha256.Sum256([]byte(storedSystem))
	sameSystem := report.System == nil || subtle.ConstantTimeCompare(storedSystemHash[:], systemHash[:]) == 1
	intervalDue := persistedLastSeen.IsZero() || now.Sub(persistedLastSeen) >= s.persistInterval
	durable := !sameState || generation != report.RunGeneration || session != report.SessionID ||
		report.ConfigRevision != appliedRevision || desired.Revision != report.ConfigRevision ||
		storedConfigError != configError || !sameSystem || intervalDue
	response := v1.ReportResponse{NodeID: nodeID, AcceptedSeq: report.Seq, ServerTime: now}
	if desired.Revision > report.ConfigRevision {
		response.DesiredConfig = &desired
	}
	duplicate := sameState && report.Seq <= cachedSeq
	if !durable {
		if charge {
			s.rateMu.Lock()
			err := s.charge(state)
			s.rateMu.Unlock()
			if err != nil {
				return v1.ReportResponse{}, err
			}
		}
		probes := state.copyProbes(storedCredential, generation, session)
		if err := probes.buffer(ctx, s.db, nodeID, report.ProbeResults); err != nil {
			return v1.ReportResponse{}, err
		}
		state.commitProbes(probes, generation, session)
		state.bufferTraffic(storedCredential, report, generation, session, now)
		response.TrafficAcks = state.trafficAcks(report.Traffic, now)
		if duplicate {
			response.AcceptedSeq = cachedSeq
			s.publicChanges.Notify()
			return response, nil
		}
		state.mu.Lock()
		state.seq, state.latest, state.receivedAt = report.Seq, body, now
		state.public = snapshotPublic(report.Metrics, now)
		state.mu.Unlock()
		s.publicChanges.Notify()
		s.recordSample(nodeID, now, report.Metrics)
		return response, nil
	}
	// This report is already tracked by beginUse; only reserve writer capacity.
	releaseWrite, err := s.queueWrite(ctx, func() {})
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer releaseWrite()
	state.stageTraffic(storedCredential, report, generation, session, now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer tx.Rollback()
	var txGenerationText, txSession, txStatus string
	var txCredential []byte
	if err := tx.QueryRowContext(ctx, "SELECT run_generation, report_session_id, token_hash, status FROM nodes WHERE id = ?", nodeID).Scan(&txGenerationText, &txSession, &txCredential, &txStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v1.ReportResponse{}, nodes.ErrUnauthenticated
		}
		return v1.ReportResponse{}, err
	}
	// Recheck in the write transaction so rotation cannot race authentication.
	if len(txCredential) == 0 || (credential != nil && subtle.ConstantTimeCompare(txCredential, credential) != 1) {
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	if txStatus == "disabled" {
		return v1.ReportResponse{}, nodes.ErrDisabled
	}
	generation, err = strconv.ParseUint(txGenerationText, 10, 64)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	state.mu.Lock()
	state.invalidateIdentity(txCredential, generation, txSession)
	hasLatest = hasLatest && len(state.latest) != 0
	state.mu.Unlock()
	if report.RunGeneration < generation || (report.RunGeneration == generation && txSession != "" && txSession != report.SessionID) {
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
	state.stageTraffic(txCredential, report, generation, txSession, now)
	desired, err = readConfig(ctx, tx, nodeID)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	// A restored database can lag the revision the agent already persisted. The
	// agent ignores revisions it has passed, so move past it and resend the
	// stored settings instead of silently dropping every later change.
	if report.ConfigRevision > desired.Revision && report.ConfigRevision < nodes.MaxConfigRevision {
		desired.Revision = report.ConfigRevision + 1
		if _, err := tx.ExecContext(ctx, "UPDATE nodes SET desired_revision = ? WHERE id = ?", desired.Revision, nodeID); err != nil {
			return v1.ReportResponse{}, err
		}
	}
	response.DesiredConfig = nil
	if desired.Revision > report.ConfigRevision {
		response.DesiredConfig = &desired
	}
	duplicate = hasLatest && subtle.ConstantTimeCompare(cachedCredential[:], txCredential) == 1 &&
		cachedGeneration == generation && cachedSession == txSession &&
		cachedGeneration == report.RunGeneration && cachedSession == report.SessionID && report.Seq <= cachedSeq
	// Flush accepted checks before adding incoming targets. This frees idle
	// slots during configuration churn without exceeding five target buffers.
	probes := state.copyProbes(txCredential, generation, txSession)
	if err := probes.persist(ctx, tx, nodeID, now); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := probes.buffer(ctx, tx, nodeID, report.ProbeResults); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := probes.persist(ctx, tx, nodeID, now); err != nil {
		return v1.ReportResponse{}, err
	}
	if duplicate {
		response.AcceptedSeq = cachedSeq
		committed, err := state.persistTraffic(ctx, tx, nodeID, now)
		if err != nil {
			return v1.ReportResponse{}, err
		}
		if err := tx.Commit(); err != nil {
			return v1.ReportResponse{}, err
		}
		state.commitTraffic(committed, now)
		state.commitProbes(probes, generation, txSession)
		response.TrafficAcks = state.trafficAcks(report.Traffic, now)
		s.publicChanges.Notify()
		return response, nil
	}
	// The transaction's identity read fenced both traffic buffers above. Flush
	// the old run and incoming run before advancing the durable process fence.
	committed, err := state.persistTraffic(ctx, tx, nodeID, now)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	// Rejection cannot erase a prior successful configuration acknowledgement.
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET run_generation = ?, report_session_id = ?, last_seen = ?, system = COALESCE(?, system), applied_revision = CASE WHEN ? = '' AND ? <= desired_revision THEN max(applied_revision, ?) ELSE applied_revision END, config_error = ? WHERE id = ?`, strconv.FormatUint(report.RunGeneration, 10), report.SessionID, timekey.Format(now), system, configError, report.ConfigRevision, report.ConfigRevision, configError, nodeID); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return v1.ReportResponse{}, err
	}
	state.commitTraffic(committed, now)
	state.commitProbes(probes, report.RunGeneration, report.SessionID)
	response.TrafficAcks = state.trafficAcks(report.Traffic, now)
	state.mu.Lock()
	state.generation, state.seq, state.latest, state.receivedAt = report.RunGeneration, report.Seq, body, now
	state.public = snapshotPublic(report.Metrics, now)
	copy(state.credential[:], txCredential)
	state.session = report.SessionID
	state.lastSeenPersisted = now
	state.mu.Unlock()
	s.publicChanges.Notify()
	s.recordSample(nodeID, now, report.Metrics)
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

// LatestReport owns a report snapshot and its server reception time.
type LatestReport struct {
	v1.Report
	ReceivedAt time.Time `json:"received_at"`
}

func (s *Service) Latest(ctx context.Context, nodeID string) (LatestReport, error) {
	// Read before taking the snapshot mutex, matching the public projection's fence.
	// This is a plain SELECT and never waits for writer admission.
	var credential []byte
	var status, generationText, session string
	if err := s.db.QueryRowContext(ctx, "SELECT token_hash, status, run_generation, report_session_id FROM nodes WHERE id = ?", nodeID).Scan(&credential, &status, &generationText, &session); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LatestReport{}, nodes.ErrNotFound
		}
		return LatestReport{}, err
	}
	if len(credential) == 0 || status == "disabled" {
		return LatestReport{}, nodes.ErrNotFound
	}
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil {
		return LatestReport{}, err
	}
	state := s.acquireExistingState(nodeID)
	if state == nil {
		return LatestReport{}, nodes.ErrNotFound
	}
	defer s.releaseState(state)
	state.mu.Lock()
	if !state.matchesIdentity(credential, generation, session) {
		state.mu.Unlock()
		return LatestReport{}, nodes.ErrNotFound
	}
	body := append([]byte(nil), state.latest...)
	receivedAt := state.receivedAt
	state.mu.Unlock()
	if len(body) == 0 {
		return LatestReport{}, nodes.ErrNotFound
	}
	var report v1.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return LatestReport{}, err
	}
	return LatestReport{Report: report, ReceivedAt: receivedAt}, nil
}

// SamplesDropped counts accepted reports omitted from best-effort history.
func (s *Service) SamplesDropped() uint64 { return s.samplesDropped.Load() }

func (s *Service) recordSample(nodeID string, now time.Time, metrics v1.Metrics) {
	if s.samples == nil {
		return
	}
	sample := history.Snapshot(nodeID, now, metrics)
	select {
	case s.samples <- sample:
	default:
		s.samplesDropped.Add(1)
	}
}
