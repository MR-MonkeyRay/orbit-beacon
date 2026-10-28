package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"orbitbeacon/internal/server/inbox"
)

func insertInbox(ctx context.Context, tx *sql.Tx, n Notification, created time.Time) error {
	details, err := json.Marshal(struct {
		RuleID     string     `json:"rule_id"`
		RuleName   string     `json:"rule_name"`
		NodeID     string     `json:"node_id"`
		NodeName   string     `json:"node_name"`
		Metric     string     `json:"metric"`
		Severity   string     `json:"severity"`
		Transition string     `json:"transition"`
		Value      *float64   `json:"value"`
		StartedAt  time.Time  `json:"started_at"`
		ResolvedAt *time.Time `json:"resolved_at"`
	}{RuleID: n.Rule.ID, RuleName: n.Rule.Name, NodeID: n.NodeID, NodeName: n.NodeName, Metric: n.Rule.Metric, Severity: n.Rule.Severity, Transition: n.State, Value: n.Value, StartedAt: n.StartedAt, ResolvedAt: n.ResolvedAt})
	if err != nil {
		return err
	}
	return inbox.Insert(ctx, tx, inbox.Event{Kind: "alert", SourceID: n.EventID, Transition: n.State, Severity: n.Rule.Severity, CreatedAt: created, Details: details})
}
