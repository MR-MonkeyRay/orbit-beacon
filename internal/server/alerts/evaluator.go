package alerts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/commands"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
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

// Notifier 在调用方事务内持久化投递，失败时由调用方回滚告警变化。
// Wake 仅在提交后调用，必须非阻塞；即使唤醒丢失，投递也由持久化轮询兜底。
type Notifier interface {
	Enqueue(context.Context, *sql.Tx, Notification) error
	Wake()
}
type NoopNotifier struct{}

func (NoopNotifier) Enqueue(context.Context, *sql.Tx, Notification) error { return nil }
func (NoopNotifier) Wake()                                                {}

type LatestReader interface {
	Latest(context.Context, string) (ingest.LatestReport, error)
}
type EvaluatorOptions struct {
	Quota    func(context.Context, time.Time) (map[string]float64, error)
	Interval time.Duration
	Now      func() time.Time
	Notifier Notifier
	Logger   *slog.Logger
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
	stopped            bool
}

type Evaluator struct {
	service     *Service
	latest      LatestReader
	opts        EvaluatorOptions
	mu          sync.Mutex // Evaluate 串行执行，包括显式的测试/手动 tick。
	pending     map[pair]pending
	resolutions map[pair]pendingResolution
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
	return &Evaluator{service: service, latest: latest, opts: opts, pending: make(map[pair]pending), resolutions: make(map[pair]pendingResolution)}
}

// Run 持有评估循环；外发由通知服务的持久化 worker 处理。
// 调用方须先取消 ctx 并等待 Run 结束，再关闭数据库。
func (e *Evaluator) Run(ctx context.Context) {
	tick := func() {
		if err := e.Evaluate(ctx, e.opts.Now()); err != nil && ctx.Err() == nil {
			if e.opts.Logger != nil {
				e.opts.Logger.LogAttrs(ctx, slog.LevelWarn, "alert_evaluation_failed", logging.ErrorAttrs(err, "evaluation_failed")...)
			}
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
func definition(r Rule) string {
	// 名称、严重级别和时间戳变更不重置待触发计时。条件或范围变更
	// 会开始新一轮告警，并结束此前仍在触发中的事件。
	// 通知路由变更不得重置待触发计时或未结束的事件。
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
	rows, err := e.service.db.QueryContext(ctx, "SELECT id,name,status,group_id,first_enrolled_at,last_seen,token_hash IS NOT NULL,token_hash,credential_epoch,run_generation,report_session_id, "+commands.StoppedSQL+" FROM nodes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []evaluationNode{}
	for rows.Next() {
		var n evaluationNode
		var enrolled, last *string
		if err := rows.Scan(&n.id, &n.name, &n.status, &n.group, &enrolled, &last, &n.credential, &n.identity, &n.credentialEpoch, &n.generation, &n.session, &n.stopped); err != nil {
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
	if r.Metric == "traffic_quota_percent" {
		latest.SessionID = ""
	}
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

// Evaluate 执行一次确定性的评估。待触发状态和待重试的恢复操作数量受当前规则/节点对数限制；
// 每次重新加载未结束的事件，避免重启后重复创建事件。
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
	// 在任何写入可能失败之前，先按当前清单修剪。后续的观测会直接更新各个规则/节点对，
	// 因此不相关的失败不会撤销已重置的状态。
	for key := range e.pending {
		r, exists := ruleMap[key.rule]
		n, present := nodeMap[key.node]
		if !exists || !present || !matches(r, n) {
			delete(e.pending, key)
		}
	}
	var evaluationErr error
	var quotas map[string]float64
	var quotaErr error
	for _, r := range rules {
		if r.Enabled && r.Metric == "traffic_quota_percent" {
			if e.opts.Quota == nil {
				quotaErr = errors.New("traffic quota reader unavailable")
			} else {
				quotas, quotaErr = e.opts.Quota(ctx, now)
			}
			evaluationErr = errors.Join(evaluationErr, quotaErr)
			break
		}
	}
	// 每个待重试的恢复操作都对应一个未结束的事件。先重试再评估当前条件，
	// 避免再次越限抹掉已观测到的恢复。
	for key, intent := range e.resolutions {
		if o, exists := open[key]; !exists || o.ID != intent.eventID {
			delete(e.resolutions, key)
		}
	}
	// 这也能处理进程停机期间被删除的情况。
	for key, o := range open {
		if o.rule.Metric == "traffic_quota_percent" && quotaErr != nil {
			continue
		}
		r, exists := ruleMap[key.rule]
		n, present := nodeMap[key.node]
		_, retry := e.resolutions[key]
		if retry || !exists || !present || !matches(r, n) || definition(r) != definition(o.rule) {
			delete(e.pending, key)
			if err := e.resolve(ctx, o, nil, now); err != nil {
				if !exists || !present {
					// 已移除的规则/节点对不保留恢复意图，后续直接从未结束的事件重试。
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
		}
		for _, r := range rules {
			if !matches(r, n) {
				continue
			}
			key := pair{r.ID, n.id}
			if r.Metric == "traffic_quota_percent" {
				if quotaErr != nil {
					delete(e.pending, key)
					continue
				}
			} else if latestErr != nil && !errors.Is(latestErr, nodes.ErrNotFound) {
				delete(e.pending, key)
				continue
			}
			o, firing := open[key]
			if firing {
				delete(e.pending, key)
				if _, retry := e.resolutions[key]; retry {
					// 上方恢复操作失败，下一次评估继续重试此事件。
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
				if r.Metric == "traffic_quota_percent" {
					v, hasQuota := quotas[n.id]
					disabled := latestErr == nil && metricDisabled(r.Metric, latest.Runtime)
					if !hasQuota || (disabled && latestFresh(latest, now)) {
						delete(e.pending, key)
						if firing {
							if err := e.resolve(ctx, o, nil, now); err != nil {
								evaluationErr = errors.Join(evaluationErr, err)
							}
						}
						continue
					}
					// 关闭配置可阻止新告警；恢复事件须有新鲜报告作证。
					if disabled {
						delete(e.pending, key)
						continue
					}
					value = &v
				} else if latestErr == nil {
					value = metricValue(latest, r.Metric, now)
				}
				// 数据缺失或过期会打断持续越限计时，但不能据此判定告警已恢复。
				if value == nil {
					delete(e.pending, key)
					// 本地关闭后不会再有数据，不恢复的话事件会一直处于触发状态。
					if firing && latestErr == nil && latestFresh(latest, now) && metricDisabled(r.Metric, latest.Runtime) {
						if err := e.resolve(ctx, o, nil, now); err != nil {
							evaluationErr = errors.Join(evaluationErr, err)
						}
					}
					continue
				}
				trueCondition = condition(r, *value, firing)
			}
			if r.Metric == "offline" && n.stopped {
				trueCondition = false
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
			// 若并发修改元数据或身份导致事务跳过此快照，保留条件已持续的时间。
			// 下一次评估会先比较规则定义和身份，再决定是否复用该计时状态。
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
	id, err := uuid.UUID(rand.Reader)
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
	// 在写锁之下重新验证规则版本、清单和范围。在本事务之前已提交的规则编辑/删除
	// 或节点停用均不会触发。
	result, err := tx.ExecContext(ctx, `INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,rule_name,node_name,rule_snapshot)
 SELECT ?,r.id,nodes.id,'firing',?,?,r.name,nodes.name,? FROM alert_rules r,nodes
 WHERE r.id=? AND r.updated_at=? AND r.enabled=1 AND nodes.id=? AND nodes.status!='disabled'
 AND nodes.token_hash=? AND nodes.credential_epoch=? AND nodes.run_generation=? AND nodes.report_session_id=? AND nodes.first_enrolled_at=?
 AND (r.scope_kind='all' OR (r.scope_kind='node' AND r.scope_id=nodes.id) OR (r.scope_kind='group' AND r.scope_id=nodes.group_id))
 AND (r.metric!='offline' OR NOT `+commands.StoppedSQL+`)
 AND NOT EXISTS(SELECT 1 FROM alert_events WHERE rule_id=r.id AND node_id=nodes.id AND state='firing')`, id, value, timekey.Format(since), string(raw), r.ID, timekey.Format(r.UpdatedAt), n.id, n.identity, n.credentialEpoch, n.generation, n.session, timekey.Format(*n.enrolled))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		notification := Notification{EventID: id, Rule: r, NodeID: n.id, NodeName: n.name, State: "firing", Value: value, StartedAt: since}
		if err := insertInbox(ctx, tx, notification, e.opts.Now()); err != nil {
			return err
		}
		if err := e.opts.Notifier.Enqueue(ctx, tx, notification); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if count > 0 {
		e.opts.Notifier.Wake()
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
	// 即使后续某个 tick 再次越限，也保留最初观测到的恢复。
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
		notification := Notification{EventID: o.ID, Rule: o.rule, NodeID: o.NodeID, NodeName: o.NodeName, State: "resolved", Value: value, StartedAt: o.StartedAt, ResolvedAt: &now}
		if err := insertInbox(ctx, tx, notification, e.opts.Now()); err != nil {
			return err
		}
		if err := e.opts.Notifier.Enqueue(ctx, tx, notification); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	delete(e.resolutions, key)
	if count > 0 {
		e.opts.Notifier.Wake()
	}
	return nil
}
