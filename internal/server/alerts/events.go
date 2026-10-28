package alerts

import (
	"context"
	"database/sql"
	"time"

	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/timekey"
)

type Event struct {
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
	err := row.Scan(&e.ID, &e.RuleID, &e.NodeID, &e.RuleName, &e.NodeName, &e.State, &e.Value, &start, &resolved, &notified, &e.Metric, &e.Severity)
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

const eventColumns = "e.id,e.rule_id,e.node_id,COALESCE(r.name,e.rule_name),COALESCE(n.name,e.node_name),e.state,e.value,e.started_at,e.resolved_at,e.last_notified_at"
const eventSelect = "SELECT " + eventColumns + ",COALESCE(json_extract(e.rule_snapshot,'$.metric'),''),COALESCE(json_extract(e.rule_snapshot,'$.severity'),'') FROM alert_events e LEFT JOIN alert_rules r ON r.id=e.rule_id LEFT JOIN nodes n ON n.id=e.node_id"

// Summary reads the counts and newest episodes from one consistent snapshot.
func (s *Service) Summary(ctx context.Context) (AlertSummary, error) {
	// The SQLite driver uses plain BEGIN for ReadOnly transactions, bypassing
	// the main pool's _txlock=immediate so this never reserves the writer.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AlertSummary{}, err
	}
	defer tx.Rollback()
	summary := AlertSummary{Newest: make([]Event, 0, 5)}
	counts := &summary.Firing
	err = tx.QueryRowContext(ctx, `SELECT count(*),
 count(CASE WHEN json_extract(rule_snapshot,'$.severity')='info' THEN 1 END),
 count(CASE WHEN json_extract(rule_snapshot,'$.severity')='warning' THEN 1 END),
 count(CASE WHEN json_extract(rule_snapshot,'$.severity')='critical' THEN 1 END)
 FROM alert_events WHERE state='firing'`).Scan(&counts.Total, &counts.Info, &counts.Warning, &counts.Critical)
	if err != nil {
		return AlertSummary{}, err
	}
	counts.Unknown = counts.Total - counts.Info - counts.Warning - counts.Critical
	rows, err := tx.QueryContext(ctx, eventSelect+" WHERE e.state='firing' ORDER BY e.started_at DESC,e.id DESC LIMIT 5")
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
	if state == "" {
		state = "all"
	}
	if state != "all" && state != "firing" && state != "resolved" {
		return EventPage{}, invalid("state", "must be firing, resolved or all")
	}
	if limit < 1 || limit > 100 {
		return EventPage{}, invalid("limit", "must be between 1 and 100")
	}
	// Metric and severity come from the snapshot taken at firing, so history
	// stays meaningful after the rule is edited or deleted.
	query := eventSelect + " WHERE 1=1"
	args := []any{}
	if state != "all" {
		query += " AND e.state=?"
		args = append(args, state)
	}
	if before != "" {
		stamp, id, err := management.DecodeEventCursor(before)
		if err != nil {
			return EventPage{}, invalid("before", "invalid cursor")
		}
		query += " AND (e.started_at,e.id)<(?,?)"
		args = append(args, stamp, id)
	}
	query += " ORDER BY e.started_at DESC,e.id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
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
	if limit < 1 {
		return 0, invalid("limit", "must be positive")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM alert_events WHERE id IN (SELECT id FROM alert_events WHERE state='resolved' AND resolved_at<? ORDER BY resolved_at,id LIMIT ?)`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
