// Package inbox 存储共享的不可变通知及每用户的已读状态。
package inbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

var (
	ErrNotFound     = errors.New("notification not found")
	ErrInvalidInput = errors.New("invalid notification input")
)

type Event struct {
	ID         string
	Kind       string
	SourceID   string
	Transition string
	Severity   string
	CreatedAt  time.Time
	Details    json.RawMessage
}

type Notification struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	SourceID   string          `json:"source_id"`
	Transition string          `json:"transition"`
	Severity   string          `json:"severity"`
	CreatedAt  time.Time       `json:"created_at"`
	Details    json.RawMessage `json:"details"`
	Read       bool            `json:"read"`
}

type Identity struct {
	UserID string
}
type Filter struct {
	Limit  int
	Before string
	Unread bool
}
type Page struct {
	Notifications []Notification `json:"notifications"`
	HasMore       bool           `json:"has_more"`
	NextCursor    string         `json:"next_cursor,omitempty"`
	ReadThrough   *string        `json:"read_through"`
}
type Summary struct {
	UnreadCount int64   `json:"unread_count"`
	LatestID    *string `json:"latest_id"`
}
type Service struct{ db *sql.DB }

func New(db *sql.DB) *Service { return &Service{db: db} }

// Insert 在来源操作的事务内插入通知。同一来源的状态转换重复处理时保持幂等；
// 其他约束冲突仍会中止调用方的事务。
func Insert(ctx context.Context, tx *sql.Tx, event Event) error {
	if (event.Kind != "task" && event.Kind != "alert") || event.SourceID == "" || event.Transition == "" || event.CreatedAt.IsZero() {
		return ErrInvalidInput
	}
	if len(event.Details) == 0 {
		event.Details = json.RawMessage(`{}`)
	}
	if ValidateDetails(event.Kind, event.Details) != nil {
		return ErrInvalidInput
	}
	if event.ID == "" {
		var err error
		event.ID, err = uuid.UUID(rand.Reader)
		if err != nil {
			return err
		}
	}
	if !uuid.ValidUUID(event.ID) {
		return ErrInvalidInput
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO inbox_notifications(id,kind,source_id,transition,severity,created_at,details_json) VALUES(?,?,?,?,?,?,?) ON CONFLICT(kind,source_id,transition) DO NOTHING`, event.ID, event.Kind, event.SourceID, event.Transition, event.Severity, timekey.Format(event.CreatedAt), string(event.Details))
	return err
}

const selectNotification = `SELECT n.id,n.kind,n.source_id,n.transition,n.severity,n.created_at,n.details_json,EXISTS(SELECT 1 FROM inbox_reads r WHERE r.user_id=? AND r.notification_id=n.id) FROM inbox_notifications n`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Notification, error) {
	var n Notification
	var created, details string
	err := row.Scan(&n.ID, &n.Kind, &n.SourceID, &n.Transition, &n.Severity, &created, &details, &n.Read)
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	if err != nil {
		return n, err
	}
	n.CreatedAt, err = timekey.Parse(created)
	n.Details = json.RawMessage(details)
	return n, err
}

func latest(ctx context.Context, tx *sql.Tx) (*string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT n.id FROM inbox_notifications n ORDER BY n.sequence DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Service) List(ctx context.Context, who Identity, filter Filter) (Page, error) {
	if who.UserID == "" || filter.Limit < 1 || filter.Limit > 100 {
		return Page{}, ErrInvalidInput
	}
	query := selectNotification
	args := []any{who.UserID}
	condition := ` WHERE `
	if filter.Unread {
		query += condition + `NOT EXISTS(SELECT 1 FROM inbox_reads r WHERE r.user_id=? AND r.notification_id=n.id)`
		condition = ` AND `
		args = append(args, who.UserID)
	}
	if filter.Before != "" {
		at, id, err := management.DecodeEventCursor(filter.Before)
		if err != nil {
			return Page{}, ErrInvalidInput
		}
		query += condition + `(n.created_at,n.id)<(?,?)`
		args = append(args, at, id)
	}
	query += ` ORDER BY n.created_at DESC,n.id DESC LIMIT ?`
	args = append(args, filter.Limit+1)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page{}, err
	}
	defer tx.Rollback()
	through, err := latest(ctx, tx)
	if err != nil {
		return Page{}, err
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, err
	}
	page := Page{Notifications: make([]Notification, 0), ReadThrough: through}
	for rows.Next() {
		n, e := scan(rows)
		if e != nil {
			rows.Close()
			return Page{}, e
		}
		page.Notifications = append(page.Notifications, n)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return Page{}, err
	}
	if len(page.Notifications) > filter.Limit {
		page.HasMore = true
		page.Notifications = page.Notifications[:filter.Limit]
		last := page.Notifications[len(page.Notifications)-1]
		page.NextCursor = management.EncodeEventCursor(last.CreatedAt, last.ID)
	}
	if err = tx.Commit(); err != nil {
		return Page{}, err
	}
	return page, nil
}

func (s *Service) Summary(ctx context.Context, who Identity) (Summary, error) {
	if who.UserID == "" {
		return Summary{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Summary{}, err
	}
	defer tx.Rollback()
	var sum Summary
	sum.LatestID, err = latest(ctx, tx)
	if err != nil {
		return Summary{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_notifications n WHERE NOT EXISTS(SELECT 1 FROM inbox_reads r WHERE r.user_id=? AND r.notification_id=n.id)`, who.UserID).Scan(&sum.UnreadCount)
	if err != nil {
		return Summary{}, err
	}
	if err = tx.Commit(); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

func (s *Service) Detail(ctx context.Context, who Identity, id string) (Notification, error) {
	if who.UserID == "" {
		return Notification{}, ErrInvalidInput
	}
	return scan(s.db.QueryRowContext(ctx, selectNotification+` WHERE n.id=?`, who.UserID, id))
}

func (s *Service) MarkRead(ctx context.Context, who Identity, id string, now time.Time) error {
	if who.UserID == "" {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found string
	if err = tx.QueryRowContext(ctx, `SELECT n.id FROM inbox_notifications n WHERE n.id=?`, id).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_reads(user_id,notification_id,read_at) VALUES(?,?,?) ON CONFLICT(user_id,notification_id) DO NOTHING`, who.UserID, id, timekey.Format(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) MarkAllRead(ctx context.Context, who Identity, through string, now time.Time) error {
	if who.UserID == "" {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT n.sequence FROM inbox_notifications n WHERE n.id=?`, through).Scan(&sequence); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_reads(user_id,notification_id,read_at) SELECT ?,n.id,? FROM inbox_notifications n WHERE n.sequence<=? ON CONFLICT(user_id,notification_id) DO NOTHING`, who.UserID, timekey.Format(now), sequence)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Cleanup 删除有界批次；保留期限由调用方控制。
func (s *Service) Cleanup(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 500 {
		return 0, ErrInvalidInput
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM inbox_notifications WHERE id IN (SELECT id FROM inbox_notifications WHERE created_at<? ORDER BY created_at,id LIMIT ?)`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
