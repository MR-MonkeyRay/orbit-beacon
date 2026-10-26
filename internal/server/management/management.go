package management

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/timekey"
)

var (
	ErrNotFound     = errors.New("management resource not found")
	ErrInvalidInput = errors.New("invalid management input")
)

type Service struct{ db *sql.DB }

func NewService(db *sql.DB) *Service { return &Service{db: db} }

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Event struct {
	ID        string          `json:"id"`
	ActorID   *string         `json:"actor_id"`
	Action    string          `json:"action"`
	TargetID  *string         `json:"target_id"`
	CreatedAt time.Time       `json:"created_at"`
	Details   json.RawMessage `json:"details"`
}

func scanGroup(row interface{ Scan(...any) error }) (Group, error) {
	var group Group
	err := row.Scan(&group.ID, &group.Name)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return group, err
}

func (s *Service) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name FROM groups ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Service) Group(ctx context.Context, id string) (Group, error) {
	return scanGroup(s.db.QueryRowContext(ctx, "SELECT id, name FROM groups WHERE id = ?", id))
}

func (s *Service) SaveGroup(ctx context.Context, id, name string) (Group, error) {
	if strings.TrimSpace(name) == "" || len(name) > 255 {
		return Group{}, ErrInvalidInput
	}
	action := "group.update"
	create := id == ""
	if create {
		var err error
		id, err = identity.UUID(rand.Reader)
		if err != nil {
			return Group{}, err
		}
		action = "group.create"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()
	var duplicate int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM groups WHERE name = ? AND id <> ?", name, id).Scan(&duplicate); err != nil {
		return Group{}, err
	}
	if duplicate != 0 {
		return Group{}, ErrInvalidInput
	}
	if create {
		_, err = tx.ExecContext(ctx, "INSERT INTO groups(id, name) VALUES (?, ?)", id, name)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE groups SET name = ? WHERE id = ?", name, id)
		if err == nil {
			err = requireRow(result)
		}
	}
	if err != nil {
		return Group{}, err
	}
	if err := audit.Insert(ctx, tx, action, id, time.Now()); err != nil {
		return Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return Group{}, err
	}
	return Group{ID: id, Name: name}, nil
}

func requireRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) DeleteGroup(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// ON DELETE SET NULL detaches nodes and enrollment references in this same transaction.
	result, err := tx.ExecContext(ctx, "DELETE FROM groups WHERE id = ?", id)
	if err != nil {
		return err
	}
	if err := requireRow(result); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "group.delete", id, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Events(ctx context.Context, limit, offset int) ([]Event, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, actor_id, action, target_id, created_at, details FROM audit_events ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var created, details string
		if err := rows.Scan(&event.ID, &event.ActorID, &event.Action, &event.TargetID, &created, &details); err != nil {
			return nil, err
		}
		event.CreatedAt, err = timekey.Parse(created)
		if err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		event.Details = json.RawMessage(details)
		events = append(events, event)
	}
	return events, rows.Err()
}

// CleanupAuditEvents removes at most limit events older than before. The
// caller owns the retention policy; this method only provides bounded,
// context-cancelable deletion.
func (s *Service) CleanupAuditEvents(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 {
		return 0, ErrInvalidInput
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM audit_events
WHERE id IN (
	SELECT id FROM audit_events
	WHERE created_at < ?
	ORDER BY created_at ASC, id ASC
	LIMIT ?
	)`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
