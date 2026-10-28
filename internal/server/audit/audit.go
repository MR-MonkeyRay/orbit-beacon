package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/timekey"
)

type actorKey struct{}

type systemActorKey struct{}

// WithActor attributes mutations to the authenticated session user.
func WithActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, actorKey{}, userID)
}

// WithSystemActor attributes mutations made by a non-user principal, such as an
// agent presenting an enrollment ticket. The event has no actor_id; its details
// record the principal kind instead.
func WithSystemActor(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, systemActorKey{}, kind)
}

// Details are small, credential-free facts about a change. Values must never
// contain secrets, tokens or password material.
type Details map[string]any

// Insert writes the event inside the mutation's transaction. Internal bootstrap
// and fixture operations without an actor do not generate events.
func Insert(ctx context.Context, tx *sql.Tx, action, targetID string, now time.Time) error {
	return InsertDetails(ctx, tx, action, targetID, now, nil)
}

// InsertDetails is Insert with additional structured details.
func InsertDetails(ctx context.Context, tx *sql.Tx, action, targetID string, now time.Time, details Details) error {
	var actor any
	if actorID, _ := ctx.Value(actorKey{}).(string); actorID != "" {
		actor = actorID
	} else if kind, _ := ctx.Value(systemActorKey{}).(string); kind != "" {
		if details == nil {
			details = Details{}
		}
		details["actor_kind"] = kind
	} else {
		return nil
	}
	encoded := []byte("{}")
	if len(details) != 0 {
		var err error
		if encoded, err = json.Marshal(details); err != nil {
			return err
		}
	}
	id, err := identity.UUID(rand.Reader)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(id, actor_id, action, target_id, created_at, details) VALUES (?, ?, ?, ?, ?, ?)", id, actor, action, targetID, timekey.Format(now), string(encoded))
	return err
}

// ScopedAlertRuleDeletes attributes rule deletions caused by an inventory scope
// cascade to the same actor and transaction as the parent deletion.
func ScopedAlertRuleDeletes(ctx context.Context, tx *sql.Tx, kind, scopeID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM alert_rules WHERE scope_kind=? AND scope_id=?", kind, scopeID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := InsertDetails(ctx, tx, "alert_rule.delete", id, now, Details{"scope_kind": kind, "scope_id": scopeID}); err != nil {
			return err
		}
	}
	return nil
}
