package management

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

var (
	ErrNotFound     = errors.New("management resource not found")
	ErrInvalidInput = errors.New("invalid management input")
)

type Service struct {
	db            *storage.Database
	publicChanged func()
}

// SetPublicInvalidator configures group-cache invalidation before serving requests.
func (s *Service) SetPublicInvalidator(changed func()) { s.publicChanged = changed }
func (s *Service) invalidatePublic() {
	if s.publicChanged != nil {
		s.publicChanged()
	}
}

func NewService(db *storage.Database) *Service { return &Service{db: db} }

type Group struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DisplayOrder int    `json:"display_order"`
}

type Event struct {
	ID        string          `json:"id"`
	ActorID   *string         `json:"actor_id"`
	Action    string          `json:"action"`
	TargetID  *string         `json:"target_id"`
	CreatedAt time.Time       `json:"created_at"`
	Details   json.RawMessage `json:"details"`
}

type EventPage struct {
	Events     []Event `json:"events"`
	HasMore    bool    `json:"has_more"`
	NextOffset *int    `json:"next_offset,omitempty"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// 事件游标使用与 SQLite 排序相同的定宽 UTC 时间戳。
func eventCursor(event Event) string {
	return EncodeEventCursor(event.CreatedAt, event.ID)
}

// EncodeEventCursor 编码事件 API 共用的时间戳/UUID 键集分页位置。
func EncodeEventCursor(created time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(timekey.Format(created) + "|" + id))
}

// DecodeEventCursor 仅接受规范的时间戳/UUID 分页位置。
func DecodeEventCursor(cursor string) (string, string, error) {
	// 规范的时间戳、分隔符和 UUID 编码后恰为 90 个字符。
	if len(cursor) != 90 {
		return "", "", ErrInvalidInput
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != cursor {
		return "", "", ErrInvalidInput
	}
	created, id, ok := strings.Cut(string(raw), "|")
	if !ok || !uuid.ValidUUID(id) {
		return "", "", ErrInvalidInput
	}
	parsed, err := timekey.Parse(created)
	if err != nil || timekey.Format(parsed) != created {
		return "", "", ErrInvalidInput
	}
	return created, id, nil
}

func scanGroup(row interface{ Scan(...any) error }) (Group, error) {
	var group Group
	err := row.Scan(&group.ID, &group.Name, &group.DisplayOrder)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return group, err
}

func (s *Service) Groups(ctx context.Context) ([]Group, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	rows, err := lease.QueryContext(ctx, "SELECT id, name, display_order FROM `groups` ORDER BY display_order DESC, name, id")
	if err != nil {
		return nil, err
	}
	return scanGroups(rows)
}

func scanGroups(rows *storage.Rows) ([]Group, error) {
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
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Group{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	return scanGroup(lease.QueryRowContext(ctx, "SELECT id, name, display_order FROM `groups` WHERE id = ?", id))
}

func (s *Service) SaveGroup(ctx context.Context, id, name string) (Group, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return Group{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if strings.TrimSpace(name) == "" || len(name) > 255 {
		return Group{}, ErrInvalidInput
	}
	action := "group.update"
	create := id == ""
	if create {
		var err error
		id, err = uuid.UUID(rand.Reader)
		if err != nil {
			return Group{}, err
		}
		action = "group.create"
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()
	var duplicate int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM `groups` WHERE name = ? AND id <> ?", name, id).Scan(&duplicate); err != nil {
		return Group{}, err
	}
	if duplicate != 0 {
		return Group{}, ErrInvalidInput
	}
	if create {
		_, err = tx.ExecContext(ctx, "INSERT INTO `groups`(id, name) VALUES (?, ?)", id, name)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE `groups` SET name = ? WHERE id = ?", name, id)
		if err == nil {
			err = requireRow(result)
		}
	}
	if err != nil {
		return Group{}, err
	}
	group, err := scanGroup(tx.QueryRowContext(ctx, "SELECT id, name, display_order FROM `groups` WHERE id = ?", id))
	if err != nil {
		return Group{}, err
	}
	if err := audit.Insert(ctx, tx, action, id, time.Now()); err != nil {
		return Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return Group{}, err
	}
	s.invalidatePublic()
	return group, nil
}

func (s *Service) ReorderGroups(ctx context.Context, ids []string) ([]Group, error) {
	lease, err := s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if ids == nil || len(ids) > 1000000 {
		return nil, ErrInvalidInput
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, name, display_order FROM `groups`")
	if err != nil {
		return nil, err
	}
	current, err := scanGroups(rows)
	if err != nil {
		return nil, err
	}
	if len(ids) != len(current) {
		return nil, ErrInvalidInput
	}
	byID := make(map[string]Group, len(current))
	for _, group := range current {
		byID[group.ID] = group
	}
	groups := make([]Group, 0, len(ids))
	for i, id := range ids {
		group, ok := byID[id]
		if !ok {
			return nil, ErrInvalidInput
		}
		delete(byID, id)
		group.DisplayOrder = len(ids) - i
		groups = append(groups, group)
	}
	for _, group := range groups {
		if _, err := tx.ExecContext(ctx, "UPDATE `groups` SET display_order = ? WHERE id = ?", group.DisplayOrder, group.ID); err != nil {
			return nil, err
		}
	}
	if err := audit.Insert(ctx, tx, "group.reorder", "", time.Now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidatePublic()
	return groups, nil
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
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := audit.ScopedAlertRuleDeletes(ctx, tx, "group", id, time.Now()); err != nil {
		return err
	}
	// ON DELETE SET NULL 在同一事务中解除节点与注册引用的关联。
	result, err := tx.ExecContext(ctx, "DELETE FROM `groups` WHERE id = ?", id)
	if err != nil {
		return err
	}
	if err := requireRow(result); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "group.delete", id, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidatePublic()
	return nil
}

func (s *Service) Events(ctx context.Context, limit, offset int, before string) (EventPage, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return EventPage{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if limit < 1 || limit > 100 || offset < 0 {
		return EventPage{}, ErrInvalidInput
	}
	query := "SELECT id, actor_id, action, target_id, created_at, details FROM audit_events"
	var args []any
	if before != "" {
		created, id, err := DecodeEventCursor(before)
		if err != nil {
			return EventPage{}, err
		}
		query += " WHERE (created_at, id) < (?, ?)"
		args = append(args, created, id)
		offset = 0
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)
	rows, err := lease.QueryContext(ctx, query, args...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var created, details string
		if err := rows.Scan(&event.ID, &event.ActorID, &event.Action, &event.TargetID, &created, &details); err != nil {
			return EventPage{}, err
		}
		event.CreatedAt, err = timekey.Parse(created)
		if err != nil {
			return EventPage{}, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		event.Details = json.RawMessage(details)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, err
	}
	page := EventPage{Events: events, HasMore: len(events) > limit}
	if page.HasMore {
		page.Events = events[:limit]
		next := offset + limit
		page.NextOffset = &next
		page.NextCursor = eventCursor(page.Events[limit-1])
	}
	return page, nil
}

// CleanupAuditEvents 最多删除 before 之前的 limit 条事件。
// 保留策略由调用方决定；本方法仅提供有界、可随 context 取消的删除。
func (s *Service) CleanupAuditEvents(ctx context.Context, before time.Time, limit int) (int64, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return 0, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if limit < 1 {
		return 0, ErrInvalidInput
	}
	tx, txErr := lease.BeginWrite(ctx)
	if txErr != nil {
		return 0, txErr
	}
	defer tx.Rollback()
	count, err := storage.DeleteBatch(ctx, tx, `SELECT id FROM audit_events WHERE created_at<? ORDER BY created_at,id LIMIT ?`, `DELETE FROM audit_events WHERE id=?`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, commitErr
	}
	return count, nil
}
