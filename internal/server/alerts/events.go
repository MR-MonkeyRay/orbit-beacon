package alerts

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
)

type Event struct {
	Target         *string    `json:"target"`
	Silenced       bool       `json:"silenced"`
	ID             string     `json:"id"`
	RuleID         string     `json:"rule_id"`
	NodeID         string     `json:"node_id"`
	RuleName       string     `json:"rule_name"`
	Metric         string     `json:"metric"`
	Severity       string     `json:"severity"`
	NodeName       string     `json:"node_name"`
	State          string     `json:"state"`
	Value          *float64   `json:"value"`
	StartedAt      time.Time  `json:"started_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	LastNotifiedAt *time.Time `json:"last_notified_at"`
}
type EventPage struct {
	Events     []Event `json:"events"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type FiringCounts struct {
	Total    int64 `json:"total"`
	Info     int64 `json:"info"`
	Warning  int64 `json:"warning"`
	Critical int64 `json:"critical"`
	Unknown  int64 `json:"unknown"`
}

type AlertSummary struct {
	Firing FiringCounts `json:"firing"`
	Newest []Event      `json:"newest"`
}

func scanEvent(row scanner) (Event, error) {
	var e Event
	var start string
	var resolved, notified *string
	err := row.Scan(&e.ID, &e.RuleID, &e.NodeID, &e.RuleName, &e.NodeName, &e.State, &e.Value, &start, &resolved, &notified, &e.Silenced, &e.Target, &e.Metric, &e.Severity)
	if err != nil {
		return e, err
	}
	e.StartedAt, err = timekey.Parse(start)
	if err != nil {
		return e, err
	}
	for _, p := range []struct {
		raw  *string
		dest **time.Time
	}{{resolved, &e.ResolvedAt}, {notified, &e.LastNotifiedAt}} {
		if p.raw != nil {
			t, err := timekey.Parse(*p.raw)
			if err != nil {
				return e, err
			}
			*p.dest = &t
		}
	}
	return e, nil
}

const eventColumnsBase = "e.id,e.rule_id,e.node_id,COALESCE(r.name,e.rule_name),COALESCE(n.name,e.node_name),e.state,e.value,e.started_at,e.resolved_at,e.last_notified_at,e.silenced,"
const eventColumnsSQLite = eventColumnsBase + "json_extract(e.rule_snapshot,'$.target')"

// JSON_UNQUOTE 固定返回 utf8mb4_bin，须显式对齐连接排序规则以保留大小写和尾空格比较。
const eventColumnsMySQL = eventColumnsBase + "NULLIF(JSON_UNQUOTE(JSON_EXTRACT(e.rule_snapshot,'$.target')) COLLATE utf8mb4_0900_bin,'null')"
const eventTables = " FROM alert_events e LEFT JOIN alert_rules r ON r.id=e.rule_id LEFT JOIN nodes n ON n.id=e.node_id"

func eventColumns(dialect storage.Dialect) string {
	if dialect == storage.MySQL {
		return eventColumnsMySQL
	}
	return eventColumnsSQLite
}

func eventSelect(dialect storage.Dialect) string {
	if dialect == storage.MySQL {
		return "SELECT " + eventColumnsMySQL + ",COALESCE(JSON_UNQUOTE(JSON_EXTRACT(e.rule_snapshot,'$.metric')) COLLATE utf8mb4_0900_bin,''),COALESCE(JSON_UNQUOTE(JSON_EXTRACT(e.rule_snapshot,'$.severity')) COLLATE utf8mb4_0900_bin,'')" + eventTables
	}
	return "SELECT " + eventColumnsSQLite + ",COALESCE(json_extract(e.rule_snapshot,'$.metric'),''),COALESCE(json_extract(e.rule_snapshot,'$.severity'),'')" + eventTables
}

// Event 按 ID 直接读取历史事件，沿用列表中的当前名称回退规则和不可变快照字段。
func (s *Service) Event(ctx context.Context, id string) (Event, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Event{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	event, err := scanEvent(lease.QueryRowContext(ctx, eventSelect(lease.Dialect())+" WHERE e.id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return event, err
}

// Summary 从一个一致的快照中读取计数和最新事件。
func (s *Service) Summary(ctx context.Context) (AlertSummary, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return AlertSummary{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginRead(ctx)
	if err != nil {
		return AlertSummary{}, err
	}
	defer tx.Rollback()
	summary := AlertSummary{Newest: make([]Event, 0, 5)}
	counts := &summary.Firing
	severity := `json_extract(rule_snapshot,'$.severity')`
	if tx.Dialect() == storage.MySQL {
		severity = `JSON_UNQUOTE(JSON_EXTRACT(rule_snapshot,'$.severity')) COLLATE utf8mb4_0900_bin`
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*),
 count(CASE WHEN `+severity+`='info' THEN 1 END),
 count(CASE WHEN `+severity+`='warning' THEN 1 END),
 count(CASE WHEN `+severity+`='critical' THEN 1 END)
 FROM alert_events WHERE state='firing'`).Scan(&counts.Total, &counts.Info, &counts.Warning, &counts.Critical)
	if err != nil {
		return AlertSummary{}, err
	}
	counts.Unknown = counts.Total - counts.Info - counts.Warning - counts.Critical
	rows, err := tx.QueryContext(ctx, eventSelect(tx.Dialect())+" WHERE e.state='firing' ORDER BY e.started_at DESC,e.id DESC LIMIT 5")
	if err != nil {
		return AlertSummary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return AlertSummary{}, err
		}
		summary.Newest = append(summary.Newest, event)
	}
	if err := rows.Err(); err != nil {
		return AlertSummary{}, err
	}
	if err := rows.Close(); err != nil {
		return AlertSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return AlertSummary{}, err
	}
	return summary, nil
}

func (s *Service) Events(ctx context.Context, state string, limit int, before string) (EventPage, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return EventPage{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if state == "" {
		state = "all"
	}
	if state != "all" && state != "firing" && state != "resolved" {
		return EventPage{}, validation.Invalid("state", "must be firing, resolved or all")
	}
	if limit < 1 || limit > 100 {
		return EventPage{}, validation.Invalid("limit", "must be between 1 and 100")
	}
	// 指标和严重级别取自告警触发时的快照，避免规则修改或删除后改变历史含义。
	query := eventSelect(lease.Dialect()) + " WHERE 1=1"
	args := []any{}
	if state != "all" {
		query += " AND e.state=?"
		args = append(args, state)
	}
	if before != "" {
		stamp, id, err := management.DecodeEventCursor(before)
		if err != nil {
			return EventPage{}, validation.Invalid("before", "invalid cursor")
		}
		query += " AND (e.started_at,e.id)<(?,?)"
		args = append(args, stamp, id)
	}
	query += " ORDER BY e.started_at DESC,e.id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := lease.QueryContext(ctx, query, args...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	page := EventPage{Events: make([]Event, 0)}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return EventPage{}, err
		}
		page.Events = append(page.Events, e)
	}
	if err = rows.Err(); err != nil {
		return EventPage{}, err
	}
	page.HasMore = len(page.Events) > limit
	if page.HasMore {
		page.Events = page.Events[:limit]
		last := page.Events[limit-1]
		page.NextCursor = management.EncodeEventCursor(last.StartedAt, last.ID)
	}
	return page, nil
}
func (s *Service) Cleanup(ctx context.Context, before time.Time, limit int) (int64, error) {
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
	count, err := storage.DeleteBatch(ctx, tx, `SELECT id FROM alert_events WHERE state='resolved' AND resolved_at<? ORDER BY resolved_at,id LIMIT ?`, `DELETE FROM alert_events WHERE id=?`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, commitErr
	}
	return count, nil
}
