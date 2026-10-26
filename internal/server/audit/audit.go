package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/timekey"
)

type actorKey struct{}

// WithActor attributes mutations to the authenticated session user.
func WithActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, actorKey{}, userID)
}

// Insert writes the event inside the mutation's transaction. Internal bootstrap
// and fixture operations without a session actor do not generate user events.
func Insert(ctx context.Context, tx *sql.Tx, action, targetID string, now time.Time) error {
	actorID, _ := ctx.Value(actorKey{}).(string)
	if actorID == "" {
		return nil
	}
	id, err := identity.UUID(rand.Reader)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(id, actor_id, action, target_id, created_at, details) VALUES (?, ?, ?, ?, ?, ?)", id, actorID, action, targetID, timekey.Format(now), "{}")
	return err
}
