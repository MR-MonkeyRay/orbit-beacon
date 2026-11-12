package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/commands"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
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
	// Samples 通过非阻塞发送接收独立的已接受样本副本；缓冲区满时丢弃历史样本，
	// 不延迟上报处理。
	Samples chan<- history.Sample
	Now     func() time.Time
	// PersistInterval 是持久化 last_seen 的基础间隔；非正数表示 30 秒。
	// 默认另加按节点固定的 [0, PersistInterval) 偏移，在后续上报时检查到期。
	PersistInterval time.Duration
	// DisablePersistOffset 关闭固定偏移，供精确验证基础间隔的测试使用。
	DisablePersistOffset bool
}

type nodeState struct {
	mu                sync.Mutex
	acceptGate        chan struct{}
	refs              int
	touch             uint64
	credential        [sha256.Size]byte
	credentialEpoch   int64
	storageGeneration uint64
	pendingCommit     *reportCommit
	generation        uint64
	session           string
	seq               uint64
	latest            []byte
	cpuPercent        *float64
	memoryUsedPercent *float64
	receivedAt        time.Time
	lastSeenPersisted time.Time
	probes            probeBuffer
	traffic           trafficBuffer
	stagedTraffic     *trafficBuffer
	public            publicMetrics
	tokens            float64
	refilled          time.Time
}

// 这些辅助函数要求持有 state.mu。限流历史独立于进程身份，失效后仍然保留。
func (state *nodeState) matchesIdentity(credential []byte, credentialEpoch int64, generation uint64, session string) bool {
	return len(state.latest) != 0 && subtle.ConstantTimeCompare(state.credential[:], credential) == 1 && state.credentialEpoch == credentialEpoch && state.generation == generation && state.session == session
}

func (state *nodeState) invalidateStorageGeneration(generation uint64) bool {
	if state.storageGeneration == generation {
		return false
	}
	state.public = publicMetrics{}
	state.cpuPercent, state.memoryUsedPercent = nil, nil
	state.lastSeenPersisted = time.Time{}
	clear(state.traffic.committed)
	if state.stagedTraffic != nil {
		clear(state.stagedTraffic.committed)
	}
	// 已确认但回退的数据不重建；仅保留尚未提交的检查及其去重水位。
	for target, entry := range state.probes.targets {
		entry.counted = time.Time{}
		for _, result := range entry.pending {
			if result.CheckedAt.After(entry.counted) {
				entry.counted = result.CheckedAt
			}
		}
		state.probes.targets[target] = entry
	}
	return true
}

func (state *nodeState) invalidateIdentity(credential []byte, credentialEpoch int64, generation uint64, session string) {
	state.fenceTraffic(credential, credentialEpoch, generation, session)
	state.fenceProbes(credential, credentialEpoch, generation, session)
	if state.matchesIdentity(credential, credentialEpoch, generation, session) {
		return
	}
	state.credential = [sha256.Size]byte{}
	state.credentialEpoch = 0
	state.generation, state.session, state.seq = 0, "", 0
	state.latest = nil
	state.cpuPercent, state.memoryUsedPercent = nil, nil
	state.receivedAt, state.lastSeenPersisted = time.Time{}, time.Time{}
	state.public = publicMetrics{}
}

type Service struct {
	samples              chan<- history.Sample
	samplesDropped       atomic.Uint64
	publicPages          publicPageCache
	db                   *storage.Database
	now                  func() time.Time
	persistInterval      time.Duration
	disablePersistOffset bool
	mu                   sync.Mutex
	rateMu               sync.Mutex
	epoch                uint64
	states               map[string]*nodeState
	writerQueue          chan struct{}
	writer               chan struct{}
	lifecycleMu          sync.Mutex
	inflight             int
	closing              bool
	done                 chan struct{}
	doneClosed           bool
}

type stateCandidate struct {
	state *nodeState
	touch uint64
}

func NewService(db *storage.Database, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PersistInterval <= 0 {
		opts.PersistInterval = 30 * time.Second
	}
	return &Service{samples: opts.Samples, db: db, now: opts.Now, persistInterval: opts.PersistInterval, disablePersistOffset: opts.DisablePersistOffset, states: make(map[string]*nodeState), writerQueue: make(chan struct{}, writerQueueCapacity), writer: make(chan struct{}, 1), done: make(chan struct{})}
}

// 同一节点始终使用相同偏移；不依赖进程启动时间或随机状态。
func persistOffset(nodeID string, interval time.Duration) time.Duration {
	digest := sha256.Sum256([]byte(nodeID))
	return time.Duration(binary.BigEndian.Uint64(digest[:8]) % uint64(interval))
}

func (s *Service) persistenceDue(nodeID string, lastSeen, now time.Time) bool {
	if lastSeen.IsZero() {
		return true
	}
	elapsed := now.Sub(lastSeen)
	if elapsed < s.persistInterval {
		return false
	}
	// 分开比较基础间隔和偏移，避免极大间隔相加时溢出。
	return s.disablePersistOffset || elapsed-s.persistInterval >= persistOffset(nodeID, s.persistInterval)
}

// beginUse 也覆盖内存中的接受，因此 Shutdown 会排空两条路径。
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
	service           *Service
	nodeID            string
	credential        [sha256.Size]byte
	credentialEpoch   int64
	storageGeneration uint64
	usedMu            sync.Mutex
	used              bool
}

func (s *Service) AdmitToken(ctx context.Context, token string) (*Admission, error) {
	if len(token) < 32 || len(token) > 256 {
		return nil, nodes.ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var nodeID, status string
	var credentialEpoch int64
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if err := lease.QueryRowContext(ctx, "SELECT id, status, credential_epoch FROM nodes WHERE token_hash = ?", digest[:]).Scan(&nodeID, &status, &credentialEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nodes.ErrUnauthenticated
		}
		return nil, err
	}
	// 禁用节点在读取请求体或扣减限流额度之前就会被拒绝。
	if status == "disabled" {
		return nil, nodes.ErrDisabled
	}
	s.mu.Lock()
	_, exists := s.states[nodeID]
	s.mu.Unlock()
	if !exists {
		if err := s.prune(ctx, lease); err != nil {
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
	return &Admission{service: s, nodeID: nodeID, credential: digest, credentialEpoch: credentialEpoch, storageGeneration: lease.Generation()}, nil
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
	return s.acceptGeneration(ctx, admission.nodeID, admission.credential[:], admission.credentialEpoch, admission.storageGeneration, report)
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

// charge 必须在持有 rateMu 时调用。
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

type normalizedReport struct {
	v1.Report
	body       []byte
	system     any
	systemHash [sha256.Size]byte
}

func normalizeReport(report v1.Report) (normalizedReport, error) {
	if report.ProtocolVersion != 1 {
		return normalizedReport{}, ErrProtocolUnsupported
	}
	if err := report.Validate(); err != nil {
		return normalizedReport{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	if err := validateTraffic(report.Traffic); err != nil {
		return normalizedReport{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	body, err := json.Marshal(report)
	if err != nil {
		return normalizedReport{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	var system any
	var systemHash [sha256.Size]byte
	if report.System != nil {
		encoded, err := json.Marshal(report.System)
		if err != nil {
			return normalizedReport{}, err
		}
		system = string(encoded)
		systemHash = sha256.Sum256(encoded)
	}
	return normalizedReport{Report: report, body: body, system: system, systemHash: systemHash}, nil
}

type acceptanceSnapshot struct {
	credential           []byte
	epoch                int64
	generationText       string
	generation           uint64
	session              string
	cachedCredential     [sha256.Size]byte
	cachedEpoch          int64
	cachedGeneration     uint64
	cachedSession        string
	cachedSeq            uint64
	hasLatest, duplicate bool
	persistedLastSeen    time.Time
}

type reportCommit struct {
	tx                  *storage.Tx
	generation          uint64
	traffic             map[trafficKey]trafficCounters
	probes              probeBuffer
	publish             func()
	resolved, committed bool
}

// acceptGate 串行保护收据；解析时不持有本次调用的数据库租约。
func (s *Service) resolveReportCommit(ctx context.Context, state *nodeState) error {
	state.mu.Lock()
	pending := state.pendingCommit
	state.mu.Unlock()
	if pending == nil {
		return nil
	}
	committed, err := pending.tx.ResolveCommit(ctx)
	if err != nil {
		return err
	}
	pending.resolved, pending.committed = true, committed
	return nil
}

func (s *Service) finishReportCommit(state *nodeState) {
	state.mu.Lock()
	pending := state.pendingCommit
	state.mu.Unlock()
	if pending == nil || !pending.resolved {
		return
	}
	if pending.committed {
		state.commitTraffic(pending.traffic, s.now().UTC())
		state.commitProbes(pending.probes, pending.probes.generation, pending.probes.session)
		if pending.publish != nil {
			pending.publish()
		}
	}
	state.mu.Lock()
	state.pendingCommit = nil
	if pending.generation != s.db.Generation() {
		state.invalidateStorageGeneration(s.db.Generation())
	}
	state.mu.Unlock()
}

func (state *nodeState) rememberReportCommit(tx *storage.Tx, generation uint64, traffic map[trafficKey]trafficCounters, probes probeBuffer, publish func()) {
	state.mu.Lock()
	state.pendingCommit = &reportCommit{tx: tx, generation: generation, traffic: traffic, probes: probes, publish: publish}
	state.mu.Unlock()
}

func snapshotPublicFromLatest(body []byte, receivedAt time.Time) publicMetrics {
	var report v1.Report
	if json.Unmarshal(body, &report) != nil {
		return publicMetrics{}
	}
	return snapshotPublic(report.Metrics, receivedAt)
}

func (s *Service) acceptGeneration(ctx context.Context, nodeID string, credential []byte, credentialEpoch int64, admissionGeneration uint64, report v1.Report) (v1.ReportResponse, error) {
	finish, err := s.beginUse(ctx)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer finish()
	normalized, err := normalizeReport(report)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	report = normalized.Report
	state := s.acquireState(nodeID)
	defer s.releaseState(state)
	// 同一节点的上报串行处理，但已取消的请求无需等待该节点的慢写入操作。
	// mu 只用于保护下面短暂的快照复制。
	select {
	case state.acceptGate <- struct{}{}:
		defer func() { <-state.acceptGate }()
	case <-ctx.Done():
		return v1.ReportResponse{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := s.resolveReportCommit(ctx, state); err != nil {
		return v1.ReportResponse{}, err
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return v1.ReportResponse{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if admissionGeneration != 0 && admissionGeneration != lease.Generation() {
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	s.finishReportCommit(state)
	// 只读认证围栏在下面的 SELECT 处线性化。
	// 在它之前提交的凭据/状态变更总能被观察到；
	// 在它之后提交的变更在这个并发请求之后才生效。
	var generationText, session, status, storedSystem string
	var lastSeenText sql.NullString
	var storedCredential []byte
	var storedEpoch int64
	var activeCommand bool
	if err := lease.QueryRowContext(ctx, "SELECT run_generation, report_session_id, token_hash, credential_epoch, status, COALESCE(`system`, ''), last_seen, EXISTS(SELECT 1 FROM agent_commands WHERE node_id=nodes.id AND state IN ('pending','delivered')) FROM nodes WHERE id = ?", nodeID).Scan(&generationText, &session, &storedCredential, &storedEpoch, &status, &storedSystem, &lastSeenText, &activeCommand); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v1.ReportResponse{}, nodes.ErrUnauthenticated
		}
		return v1.ReportResponse{}, err
	}
	if len(storedCredential) == 0 || (credential != nil && (subtle.ConstantTimeCompare(storedCredential, credential) != 1 || storedEpoch != credentialEpoch)) {
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
	newStorageGeneration := state.invalidateStorageGeneration(lease.Generation())
	state.invalidateIdentity(storedCredential, storedEpoch, generation, session)
	cachedCredential, cachedEpoch, cachedGeneration, cachedSession := state.credential, state.credentialEpoch, state.generation, state.session
	cachedSeq, hasLatest := state.seq, len(state.latest) != 0
	state.mu.Unlock()
	now := s.now().UTC()
	if report.RunGeneration < generation || (report.RunGeneration == generation && session != "" && session != report.SessionID) {
		return v1.ReportResponse{}, ErrIdentityConflict
	}
	var persistedLastSeen time.Time
	if lastSeenText.Valid {
		persistedLastSeen, err = timekey.Parse(lastSeenText.String)
		if err != nil {
			return v1.ReportResponse{}, err
		}
	}
	// 去重既属于凭据也属于持久化进程围栏：
	// 撤销/重新注册可能复用凭据同时重置该围栏（ABA）。
	sameState := hasLatest && subtle.ConstantTimeCompare(cachedCredential[:], storedCredential) == 1 && cachedEpoch == storedEpoch &&
		cachedGeneration == generation && cachedSession == session &&
		cachedGeneration == report.RunGeneration && cachedSession == report.SessionID
	storedSystemHash := sha256.Sum256([]byte(storedSystem))
	sameSystem := report.System == nil || subtle.ConstantTimeCompare(storedSystemHash[:], normalized.systemHash[:]) == 1
	intervalDue := s.persistenceDue(nodeID, persistedLastSeen, now)
	duplicate := sameState && report.Seq <= cachedSeq
	durable := newStorageGeneration || activeCommand || len(report.CommandResults) > 0 || !sameState || generation != report.RunGeneration || session != report.SessionID ||
		(!duplicate && !sameSystem) || intervalDue
	response := v1.ReportResponse{NodeID: nodeID, AcceptedSeq: report.Seq, ServerTime: now}
	fence := acceptanceSnapshot{credential: storedCredential, epoch: storedEpoch, generationText: generationText, generation: generation, session: session,
		cachedCredential: cachedCredential, cachedEpoch: cachedEpoch, cachedGeneration: cachedGeneration, cachedSession: cachedSession, cachedSeq: cachedSeq, hasLatest: hasLatest, duplicate: duplicate, persistedLastSeen: persistedLastSeen}
	if !durable {
		return s.acceptInMemory(ctx, lease, nodeID, state, normalized, fence, response)
	}
	return s.persistReport(ctx, nodeID, credential, state, normalized, fence, response)
}

func (s *Service) acceptInMemory(ctx context.Context, lease *storage.Lease, nodeID string, state *nodeState, report normalizedReport, fence acceptanceSnapshot, response v1.ReportResponse) (v1.ReportResponse, error) {
	now := response.ServerTime
	var err error
	probes := state.copyProbes(fence.credential, fence.epoch, fence.generation, fence.session)
	targets, err := state.reportProbeTargets(report.Report, fence.duplicate)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	probes.retainTargets(targets)
	if err := probes.buffer(ctx, lease, nodeID, report.ProbeResults, targets); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return v1.ReportResponse{}, err
	}
	state.commitProbes(probes, fence.generation, fence.session)
	state.bufferTraffic(fence.credential, fence.epoch, report.Report, fence.generation, fence.session, now)
	response.TrafficAcks = state.trafficAcks(report.Traffic, now)
	if fence.duplicate {
		response.AcceptedSeq = fence.cachedSeq
		return response, nil
	}
	snapshot := snapshotPublic(report.Metrics, now)
	state.mu.Lock()
	state.seq, state.latest, state.receivedAt = report.Seq, report.body, now
	state.public = snapshot
	state.cpuPercent, state.memoryUsedPercent = liveMetricValue(snapshot.cpu), liveMetricValue(snapshot.memory)
	state.storageGeneration = lease.Generation()
	state.mu.Unlock()
	s.recordSample(nodeID, now, report.Metrics)
	return response, nil
}

func (s *Service) persistReport(ctx context.Context, nodeID string, credential []byte, state *nodeState, report normalizedReport, fence acceptanceSnapshot, response v1.ReportResponse) (v1.ReportResponse, error) {
	now := response.ServerTime
	// 该报告已被 beginUse 跟踪；仅预留写入容量。
	releaseWrite, err := s.queueWrite(ctx, func() {})
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer releaseWrite()
	state.stageTraffic(fence.credential, fence.epoch, report.Report, fence.generation, fence.session, now)
	writer, err := s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer writer.Close()
	ctx = writer.Context()
	tx, err := writer.BeginWrite(ctx)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	defer tx.Rollback()
	var txGenerationText, txSession, txStatus string
	var txCredential []byte
	var txEpoch int64
	if err := tx.QueryRowContext(ctx, "SELECT run_generation, report_session_id, token_hash, credential_epoch, status FROM nodes WHERE id = ?", nodeID).Scan(&txGenerationText, &txSession, &txCredential, &txEpoch, &txStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v1.ReportResponse{}, nodes.ErrUnauthenticated
		}
		return v1.ReportResponse{}, err
	}
	// 在写事务中重新检查，使轮换无法与认证竞争。
	if len(txCredential) == 0 || subtle.ConstantTimeCompare(txCredential, fence.credential) != 1 || (credential != nil && subtle.ConstantTimeCompare(txCredential, credential) != 1) {
		return v1.ReportResponse{}, nodes.ErrUnauthenticated
	}
	if txEpoch != fence.epoch || txGenerationText != fence.generationText || txSession != fence.session {
		return v1.ReportResponse{}, ErrIdentityConflict
	}
	if txStatus == "disabled" {
		return v1.ReportResponse{}, nodes.ErrDisabled
	}
	fence.generation, err = strconv.ParseUint(txGenerationText, 10, 64)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	state.mu.Lock()
	state.invalidateIdentity(txCredential, txEpoch, fence.generation, txSession)
	fence.hasLatest = fence.hasLatest && len(state.latest) != 0
	state.mu.Unlock()
	if report.RunGeneration < fence.generation || (report.RunGeneration == fence.generation && txSession != "" && txSession != report.SessionID) {
		return v1.ReportResponse{}, ErrIdentityConflict
	}
	state.stageTraffic(txCredential, txEpoch, report.Report, fence.generation, txSession, now)
	fence.duplicate = fence.hasLatest && subtle.ConstantTimeCompare(fence.cachedCredential[:], txCredential) == 1 && fence.cachedEpoch == txEpoch &&
		fence.cachedGeneration == fence.generation && fence.cachedSession == txSession &&
		fence.cachedGeneration == report.RunGeneration && fence.cachedSession == report.SessionID && report.Seq <= fence.cachedSeq
	response.Command, err = commands.Exchange(ctx, tx, nodeID, txEpoch, strconv.FormatUint(report.RunGeneration, 10), report.CommandResults, now)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	targets, err := state.reportProbeTargets(report.Report, fence.duplicate)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	probes := state.copyProbes(txCredential, txEpoch, fence.generation, txSession)
	probes.retainTargets(targets)
	if err := probes.persist(ctx, tx, nodeID, now); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := probes.buffer(ctx, tx, nodeID, report.ProbeResults, targets); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := probes.persist(ctx, tx, nodeID, now); err != nil {
		return v1.ReportResponse{}, err
	}
	if fence.duplicate {
		response.AcceptedSeq = fence.cachedSeq
		committed, err := state.persistTraffic(ctx, tx, nodeID, now)
		if err != nil {
			return v1.ReportResponse{}, err
		}
		if err := tx.Commit(); err != nil {
			if errors.Is(err, storage.ErrCommitUnknown) {
				probes.generation, probes.session = fence.generation, txSession
				state.rememberReportCommit(tx, writer.Generation(), committed, probes, nil)
			}
			return v1.ReportResponse{}, err
		}
		state.commitTraffic(committed, now)
		state.mu.Lock()
		state.storageGeneration = writer.Generation()
		state.lastSeenPersisted = fence.persistedLastSeen
		if state.public.receivedAt.IsZero() {
			state.public = snapshotPublicFromLatest(state.latest, state.receivedAt)
		}
		state.cpuPercent, state.memoryUsedPercent = liveMetricValue(state.public.cpu), liveMetricValue(state.public.memory)
		state.mu.Unlock()
		state.commitProbes(probes, fence.generation, txSession)
		response.TrafficAcks = state.trafficAcks(report.Traffic, now)
		return response, nil
	}
	// 已按事务内读取的身份校验上方两个流量缓冲区。
	// 推进持久化的进程隔离边界前，先写入旧运行和新运行的流量数据。
	committed, err := state.persistTraffic(ctx, tx, nodeID, now)
	if err != nil {
		return v1.ReportResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET run_generation = ?, report_session_id = ?, last_seen = ?, `system` = COALESCE(?, `system`) WHERE id = ?", strconv.FormatUint(report.RunGeneration, 10), report.SessionID, timekey.Format(now), report.system, nodeID); err != nil {
		return v1.ReportResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		if errors.Is(err, storage.ErrCommitUnknown) {
			probes.generation, probes.session = report.RunGeneration, report.SessionID
			snapshot := snapshotPublic(report.Metrics, now)
			var sample history.Sample
			if s.samples != nil {
				sample = history.Snapshot(nodeID, now, report.Metrics)
			}
			state.rememberReportCommit(tx, writer.Generation(), committed, probes, func() {
				state.mu.Lock()
				state.generation, state.seq, state.latest, state.receivedAt = report.RunGeneration, report.Seq, report.body, now
				state.storageGeneration = 0
				state.public = snapshot
				state.cpuPercent, state.memoryUsedPercent = liveMetricValue(snapshot.cpu), liveMetricValue(snapshot.memory)
				copy(state.credential[:], txCredential)
				state.credentialEpoch, state.session, state.lastSeenPersisted = txEpoch, report.SessionID, now
				state.mu.Unlock()
				if s.samples != nil {
					s.publishSample(sample)
				}
			})
		}
		return v1.ReportResponse{}, err
	}
	state.commitTraffic(committed, now)
	state.commitProbes(probes, report.RunGeneration, report.SessionID)
	response.TrafficAcks = state.trafficAcks(report.Traffic, now)
	snapshot := snapshotPublic(report.Metrics, now)
	state.mu.Lock()
	state.generation, state.seq, state.latest, state.receivedAt = report.RunGeneration, report.Seq, report.body, now
	state.storageGeneration = writer.Generation()
	state.public = snapshot
	state.cpuPercent, state.memoryUsedPercent = liveMetricValue(snapshot.cpu), liveMetricValue(snapshot.memory)
	copy(state.credential[:], txCredential)
	state.credentialEpoch = txEpoch
	state.session = report.SessionID
	state.lastSeenPersisted = now
	state.mu.Unlock()
	s.recordSample(nodeID, now, report.Metrics)
	return response, nil
}

type rowQueryer interface {
	QueryContext(context.Context, string, ...any) (*storage.Rows, error)
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
		state.mu.Lock()
		hasPending := state.pendingCommit != nil
		state.mu.Unlock()
		if state.refs == 0 && !hasPending {
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

// LatestReport 拥有报告快照及其服务端接收时间。
type LatestReport struct {
	v1.Report
	ReceivedAt time.Time `json:"received_at"`
}

func (s *Service) Latest(ctx context.Context, nodeID string) (LatestReport, error) {
	// 在获取快照互斥锁之前读取，与公开投影的围栏保持一致。
	// 这是普通 SELECT，从不等待写入准入。
	var credential []byte
	var credentialEpoch int64
	var status, generationText, session string
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return LatestReport{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if err := lease.QueryRowContext(ctx, "SELECT token_hash, credential_epoch, status, run_generation, report_session_id FROM nodes WHERE id = ?", nodeID).Scan(&credential, &credentialEpoch, &status, &generationText, &session); err != nil {
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
	if !s.db.Ready() || state.storageGeneration != lease.Generation() || lease.Generation() != s.db.Generation() || !state.matchesIdentity(credential, credentialEpoch, generation, session) {
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

// SamplesDropped 统计未写入历史记录的已接受上报数。
func (s *Service) SamplesDropped() uint64 { return s.samplesDropped.Load() }

func (s *Service) recordSample(nodeID string, now time.Time, metrics v1.Metrics) {
	if s.samples == nil {
		return
	}
	sample := history.Snapshot(nodeID, now, metrics)
	s.publishSample(sample)
}

func (s *Service) publishSample(sample history.Sample) {
	select {
	case s.samples <- sample:
	default:
		s.samplesDropped.Add(1)
	}
}

// WriteQueueDepth 返回已占用的写入槽数，包含正在写入的一项；不获取写路径的锁。
func (s *Service) WriteQueueDepth() int { return len(s.writerQueue) }
