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
	"orbitbeacon/internal/server/storage"
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
type Service struct{ db *storage.Database }

func New(db *storage.Database) *Service { return &Service{db: db} }

// Insert 在来源操作的事务内插入通知。同一来源的状态转换重复处理时保持幂等；
// 其他约束冲突仍会中止调用方的事务。
func Insert(ctx context.Context, tx *storage.Tx, event Event) error {
	if (event.Kind != "task" && event.Kind != "alert" && event.Kind != "storage" && event.Kind != "service") || event.SourceID == "" || event.Transition == "" || event.CreatedAt.IsZero() {
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
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_notifications WHERE kind=? AND source_id=? AND transition=?)`, event.Kind, event.SourceID, event.Transition).Scan(&exists); err != nil || exists {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE inbox_sequence SET value=value+1 WHERE id=1 AND value<9223372036854775807`)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errors.Join(errors.New("inbox sequence exhausted or missing"), err)
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM inbox_sequence WHERE id=1`).Scan(&sequence); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_notifications(sequence,id,kind,source_id,transition,severity,created_at,details_json) VALUES(?,?,?,?,?,?,?,?)`, sequence, event.ID, event.Kind, event.SourceID, event.Transition, event.Severity, timekey.Format(event.CreatedAt), string(event.Details))
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

func latest(ctx context.Context, tx *storage.Tx) (*string, error) {
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
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Page{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
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
	tx, err := lease.BeginRead(ctx)
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
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Summary{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if who.UserID == "" {
		return Summary{}, ErrInvalidInput
	}
	tx, err := lease.BeginRead(ctx)
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
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Notification{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if who.UserID == "" {
		return Notification{}, ErrInvalidInput
	}
	return scan(lease.QueryRowContext(ctx, selectNotification+` WHERE n.id=?`, who.UserID, id))
}

func (s *Service) MarkRead(ctx context.Context, who Identity, id string, now time.Time) error {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if who.UserID == "" {
		return ErrInvalidInput
	}
	tx, err := lease.BeginWrite(ctx)
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
	query := `INSERT INTO inbox_reads(user_id,notification_id,read_at) VALUES(?,?,?) ON CONFLICT(user_id,notification_id) DO NOTHING`
	if tx.Dialect() == storage.MySQL {
		query = `INSERT INTO inbox_reads(user_id,notification_id,read_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE read_at=inbox_reads.read_at`
	}
	if _, err = tx.ExecContext(ctx, query, who.UserID, id, timekey.Format(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) MarkAllRead(ctx context.Context, who Identity, through string, now time.Time) error {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if who.UserID == "" {
		return ErrInvalidInput
	}
	tx, err := lease.BeginWrite(ctx)
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
	query := `INSERT INTO inbox_reads(user_id,notification_id,read_at) SELECT ?,n.id,? FROM inbox_notifications n WHERE n.sequence<=? ON CONFLICT(user_id,notification_id) DO NOTHING`
	if tx.Dialect() == storage.MySQL {
		query = `INSERT INTO inbox_reads(user_id,notification_id,read_at) SELECT ?,n.id,? FROM inbox_notifications n WHERE n.sequence<=? ON DUPLICATE KEY UPDATE read_at=inbox_reads.read_at`
	}
	_, err = tx.ExecContext(ctx, query, who.UserID, timekey.Format(now), sequence)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Cleanup 删除有界批次；保留期限由调用方控制。
func (s *Service) Cleanup(ctx context.Context, before time.Time, limit int) (int64, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return 0, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if limit < 1 || limit > 500 {
		return 0, ErrInvalidInput
	}
	tx, txErr := lease.BeginWrite(ctx)
	if txErr != nil {
		return 0, txErr
	}
	defer tx.Rollback()
	count, err := storage.DeleteBatch(ctx, tx, `SELECT id FROM inbox_notifications WHERE created_at<? ORDER BY created_at,id LIMIT ?`, `DELETE FROM inbox_notifications WHERE id=?`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, commitErr
	}
	return count, nil
}
