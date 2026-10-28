package alerts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

type Notification struct {
	EventID    string
	Rule       Rule
	NodeID     string
	NodeName   string
	State      string
	Value      *float64
	StartedAt  time.Time
	ResolvedAt *time.Time
}

// Notifier runs on a separate bounded worker after commit. Implementations must
// honor context cancellation. Delivery/retries are intentionally left to part 2.
type Notifier interface {
	Notify(context.Context, Notification)
}
type NoopNotifier struct{}

func (NoopNotifier) Notify(context.Context, Notification) {}

type LatestReader interface {
	Latest(context.Context, string) (ingest.LatestReport, error)
}
type EvaluatorOptions struct {
	Interval  time.Duration
	Now       func() time.Time
	Notifier  Notifier
	QueueSize int
	Logger    *slog.Logger
}
type pair struct{ rule, node string }
type pending struct {
	since      time.Time
	definition string
}
type pendingResolution struct {
	eventID    string
	observedAt time.Time
	value      *float64
}
type openEvent struct {
	Event
	rule Rule
}
type evaluationNode struct {
	identity           []byte
	credentialEpoch    int64
	generation         string
	session            string
	id, name, status   string
	group              *string
	enrolled, lastSeen *time.Time
	credential         bool
}

type Evaluator struct {
	service     *Service
	latest      LatestReader
	opts        EvaluatorOptions
	mu          sync.Mutex // Evaluate is serialized, including explicit test/manual ticks.
	pending     map[pair]pending
	resolutions map[pair]pendingResolution
	queue       chan Notification
	dropped     atomic.Uint64
}

func NewEvaluator(service *Service, latest LatestReader, opts EvaluatorOptions) *Evaluator {
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Notifier == nil {
		opts.Notifier = NoopNotifier{}
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 256
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Evaluator{service: service, latest: latest, opts: opts, pending: make(map[pair]pending), resolutions: make(map[pair]pendingResolution), queue: make(chan Notification, opts.QueueSize)}
}
func (e *Evaluator) NotificationsDropped() uint64 { return e.dropped.Load() }

// Run owns the evaluation and notification workers and returns after both stop.
// The caller cancels ctx and waits for Run before closing the database.
func (e *Evaluator) Run(ctx context.Context) {
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-e.queue:
				if ctx.Err() != nil {
					return
				}
				e.opts.Notifier.Notify(ctx, n)
			}
		}
	}()
	defer func() { <-workerDone }()
	tick := func() {
		if err := e.Evaluate(ctx, e.opts.Now()); err != nil && ctx.Err() == nil {
			e.opts.Logger.Warn("alert evaluation failed", "error", err)
		}
	}
	tick()
	ticker := time.NewTicker(e.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}
func (e *Evaluator) enqueue(n Notification) {
	select {
	case e.queue <- n:
	default:
		e.dropped.Add(1)
		e.opts.Logger.Warn("alert notification queue full")
	}
}
func definition(r Rule) string {
	// Names/severity and timestamps do not restart pending timers. Condition and
	// scope changes start a new episode and resolve any previous firing episode.
	// Routing changes must not reset pending timers or open episodes.
	r.ChannelIDs = nil
	r.ID = ""
	r.Name = ""
	r.Severity = ""
	r.CreatedAt = time.Time{}
	r.UpdatedAt = time.Time{}
	raw, _ := json.Marshal(r)
	return string(raw)
}
func (e *Evaluator) readNodes(ctx context.Context) ([]evaluationNode, error) {
	rows, err := e.service.db.QueryContext(ctx, "SELECT id,name,status,group_id,first_enrolled_at,last_seen,token_hash IS NOT NULL,token_hash,credential_epoch,run_generation,report_session_id FROM nodes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []evaluationNode{}
	for rows.Next() {
		var n evaluationNode
		var enrolled, last *string
		if err := rows.Scan(&n.id, &n.name, &n.status, &n.group, &enrolled, &last, &n.credential, &n.identity, &n.credentialEpoch, &n.generation, &n.session); err != nil {
			return nil, err
		}
		for _, p := range []struct {
			raw  *string
			dest **time.Time
		}{{enrolled, &n.enrolled}, {last, &n.lastSeen}} {
			if p.raw != nil {
				v, err := timekey.Parse(*p.raw)
				if err != nil {
					return nil, err
				}
				*p.dest = &v
			}
		}
		result = append(result, n)
	}
	return result, rows.Err()
}
func (e *Evaluator) readOpen(ctx context.Context) (map[pair]openEvent, error) {
	rows, err := e.service.db.QueryContext(ctx, "SELECT "+eventColumns+",e.rule_snapshot FROM alert_events e LEFT JOIN alert_rules r ON r.id=e.rule_id LEFT JOIN nodes n ON n.id=e.node_id WHERE e.state='firing'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[pair]openEvent{}
	for rows.Next() {
		var o openEvent
		var start, raw string
		var resolved, notified *string
		if err := rows.Scan(&o.ID, &o.RuleID, &o.NodeID, &o.RuleName, &o.NodeName, &o.State, &o.Value, &start, &resolved, &notified, &raw); err != nil {
			return nil, err
		}
		o.StartedAt, err = timekey.Parse(start)
		if err != nil {
			return nil, err
		}
		if o.rule, err = ParseRuleSnapshot([]byte(raw)); err != nil {
			return nil, err
		}
		result[pair{o.RuleID, o.NodeID}] = o
	}
	return result, rows.Err()
}
func pendingDefinition(r Rule, n evaluationNode, latest ingest.LatestReport) string {
	return definition(r) + "|" + hex.EncodeToString(n.identity) + "|" + strconv.FormatInt(n.credentialEpoch, 10) + "|" + timekey.Format(*n.enrolled) + "|" + n.generation + "|" + n.session + "|" + latest.SessionID
}

func matches(r Rule, n evaluationNode) bool {
	if !r.Enabled || n.status == "disabled" || !n.credential || n.enrolled == nil {
		return false
	}
	switch r.ScopeKind {
	case "all":
		return true
	case "node":
		return r.ScopeID != nil && *r.ScopeID == n.id
	case "group":
		return r.ScopeID != nil && n.group != nil && *r.ScopeID == *n.group
	}
	return false
}

// Evaluate performs one deterministic tick. Pending state and resolution intents
// are bounded by current rule/node pairs; open state is reloaded so restart never
// duplicates episodes.
func (e *Evaluator) Evaluate(ctx context.Context, now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now = now.UTC()
	rules, err := e.service.List(ctx)
	if err != nil {
		return err
	}
	inventory, err := e.readNodes(ctx)
	if err != nil {
		return err
	}
	open, err := e.readOpen(ctx)
	if err != nil {
		return err
	}
	ruleMap := map[string]Rule{}
	nodeMap := map[string]evaluationNode{}
	for _, r := range rules {
		ruleMap[r.ID] = r
	}
	for _, n := range inventory {
		nodeMap[n.id] = n
	}
	// Prune from the current inventory before any write can fail. Observations
	// below update each pair directly so unrelated failures cannot undo resets.
	for key := range e.pending {
		r, exists := ruleMap[key.rule]
		n, present := nodeMap[key.node]
		if !exists || !present || !matches(r, n) {
			delete(e.pending, key)
		}
	}
	var evaluationErr error
	// Each resolution intent belongs to one open event. Retry it before evaluating
	// current conditions so a re-breach cannot erase an observed recovery.
	for key, intent := range e.resolutions {
		if o, exists := open[key]; !exists || o.ID != intent.eventID {
			delete(e.resolutions, key)
		}
	}
	// This also handles deletions while the process was stopped.
	for key, o := range open {
		r, exists := ruleMap[key.rule]
		n, present := nodeMap[key.node]
		_, retry := e.resolutions[key]
		if retry || !exists || !present || !matches(r, n) || definition(r) != definition(o.rule) {
			delete(e.pending, key)
			if err := e.resolve(ctx, o, nil, now); err != nil {
				if !exists || !present {
					// Orphans are retried from open events without retaining intents
					// for removed rule/node pairs.
					delete(e.resolutions, key)
				}
				evaluationErr = errors.Join(evaluationErr, err)
				continue
			}
			delete(open, key)
		}
	}
	for _, n := range inventory {
		var latest ingest.LatestReport
		var latestErr error
		if e.latest != nil {
			latest, latestErr = e.latest.Latest(ctx, n.id)
		} else {
			latestErr = nodes.ErrNotFound
		}
		if latestErr != nil && !errors.Is(latestErr, nodes.ErrNotFound) {
			evaluationErr = errors.Join(evaluationErr, latestErr)
			for _, r := range rules {
				delete(e.pending, pair{r.ID, n.id})
			}
			continue
		}
		for _, r := range rules {
			if !matches(r, n) {
				continue
			}
			key := pair{r.ID, n.id}
			o, firing := open[key]
			if firing {
				delete(e.pending, key)
				if _, retry := e.resolutions[key]; retry {
					// Resolution failed above; retry this episode next tick.
					continue
				}
			}
			var value *float64
			var since time.Time
			var trueCondition bool
			if r.Metric == "offline" {
				delete(e.pending, key)
				seen := *n.enrolled
				if n.lastSeen != nil && n.lastSeen.After(seen) {
					seen = *n.lastSeen
				}
				if latestErr == nil && latest.ReceivedAt.After(seen) {
					seen = latest.ReceivedAt
				}
				since = seen
				trueCondition = !seen.After(now) && now.Sub(seen) >= time.Duration(r.ForSeconds)*time.Second
			} else {
				if latestErr == nil {
					value = metricValue(latest, r.Metric, now)
				}
				// Missing/stale data breaks pending continuity, but cannot establish recovery.
				if value == nil {
					delete(e.pending, key)
					continue
				}
				trueCondition = condition(r, *value, firing)
			}
			if !trueCondition {
				delete(e.pending, key)
				if firing {
					if err := e.resolve(ctx, o, value, now); err != nil {
						evaluationErr = errors.Join(evaluationErr, err)
					}
				}
				continue
			}
			if firing {
				continue
			}
			if r.Metric != "offline" {
				p, exists := e.pending[key]
				if !exists || p.definition != pendingDefinition(r, n, latest) || p.since.After(now) {
					p = pending{since: now, definition: pendingDefinition(r, n, latest)}
				}
				since = p.since
				if now.Sub(since) < time.Duration(r.ForSeconds)*time.Second {
					e.pending[key] = p
					continue
				}
			}
			// Preserve elapsed condition time if a concurrent metadata edit or
			// identity change makes the transaction skip this snapshot. The next
			// tick compares definitions/identity before reusing this entry.
			if r.Metric != "offline" {
				e.pending[key] = pending{since: since, definition: pendingDefinition(r, n, latest)}
			}
			if err := e.fire(ctx, r, n, value, since); err != nil {
				evaluationErr = errors.Join(evaluationErr, err)
			}
		}
	}
	return evaluationErr
}
func (e *Evaluator) fire(ctx context.Context, r Rule, n evaluationNode, value *float64, since time.Time) error {
	id, err := identity.UUID(rand.Reader)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := e.service.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Revalidate rule version, inventory and scope under the writer lock. A rule
	// edit/deletion or node disable committed before this transaction cannot fire.
	result, err := tx.ExecContext(ctx, `INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,rule_name,node_name,rule_snapshot)
 SELECT ?,r.id,n.id,'firing',?,?,r.name,n.name,? FROM alert_rules r,nodes n
 WHERE r.id=? AND r.updated_at=? AND r.enabled=1 AND n.id=? AND n.status!='disabled'
 AND n.token_hash=? AND n.credential_epoch=? AND n.run_generation=? AND n.report_session_id=? AND n.first_enrolled_at=?
 AND (r.scope_kind='all' OR (r.scope_kind='node' AND r.scope_id=n.id) OR (r.scope_kind='group' AND r.scope_id=n.group_id))
 AND NOT EXISTS(SELECT 1 FROM alert_events WHERE rule_id=r.id AND node_id=n.id AND state='firing')`, id, value, timekey.Format(since), string(raw), r.ID, timekey.Format(r.UpdatedAt), n.id, n.identity, n.credentialEpoch, n.generation, n.session, timekey.Format(*n.enrolled))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		if err := insertInbox(ctx, tx, Notification{EventID: id, Rule: r, NodeID: n.id, NodeName: n.name, State: "firing", Value: value, StartedAt: since}, e.opts.Now()); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if count > 0 {
		e.enqueue(Notification{EventID: id, Rule: r, NodeID: n.id, NodeName: n.name, State: "firing", Value: value, StartedAt: since})
	}
	return nil
}
func (e *Evaluator) resolve(ctx context.Context, o openEvent, value *float64, now time.Time) error {
	key := pair{o.RuleID, o.NodeID}
	intent, exists := e.resolutions[key]
	if !exists || intent.eventID != o.ID {
		intent = pendingResolution{eventID: o.ID, observedAt: now, value: value}
		e.resolutions[key] = intent
	}
	// Keep the first observed recovery even if a later tick breaches again.
	now, value = intent.observedAt, intent.value
	tx, err := e.service.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE alert_events SET state='resolved',resolved_at=?,value=? WHERE id=? AND state='firing'", timekey.Format(now), value, o.ID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		if err := insertInbox(ctx, tx, Notification{EventID: o.ID, Rule: o.rule, NodeID: o.NodeID, NodeName: o.NodeName, State: "resolved", Value: value, StartedAt: o.StartedAt, ResolvedAt: &now}, e.opts.Now()); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	delete(e.resolutions, key)
	if count > 0 {
		e.enqueue(Notification{EventID: o.ID, Rule: o.rule, NodeID: o.NodeID, NodeName: o.NodeName, State: "resolved", Value: value, StartedAt: o.StartedAt, ResolvedAt: &now})
	}
	return nil
}
