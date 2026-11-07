package announcements

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

var ErrNotFound = errors.New("announcement not found")
var ErrCapacity = errors.New("announcement capacity reached")

// Public 只含允许匿名展示的五个字段。
type Public struct {
	Level    string     `json:"level"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
}

type Announcement struct {
	Public
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
}

type Patch struct {
	Level    json.RawMessage `json:"level"`
	Title    json.RawMessage `json:"title"`
	Body     json.RawMessage `json:"body"`
	StartsAt json.RawMessage `json:"starts_at"`
	EndsAt   json.RawMessage `json:"ends_at"`
}

// Validate 规范化展示文本并校验语法，API 和备份共用。恢复备份时允许历史窗口。
func Validate(p *Public) error {
	fields := map[string]string{}
	if p.Level != "info" && p.Level != "warning" && p.Level != "critical" {
		fields["level"] = "must be info, warning or critical"
	}
	p.Body = strings.ReplaceAll(p.Body, "\r\n", "\n")
	for _, f := range []struct {
		name      string
		value     *string
		max       int
		multiline bool
	}{
		{"title", &p.Title, 100, false}, {"body", &p.Body, 1000, true},
	} {
		// 先查控制字符再去空白，否则开头的 tab 或 CR 会被静默去掉。
		invalid := !utf8.ValidString(*f.value) || strings.ContainsFunc(*f.value, func(r rune) bool {
			return unicode.IsControl(r) && !(f.multiline && r == '\n')
		})
		*f.value = strings.TrimSpace(*f.value)
		if invalid || len(*f.value) == 0 || len(*f.value) > f.max {
			fields[f.name] = "invalid announcement " + f.name
		}
	}
	if p.StartsAt.IsZero() {
		fields["starts_at"] = "must be an RFC3339 timestamp"
	}
	if p.EndsAt != nil && !p.EndsAt.After(p.StartsAt) {
		fields["ends_at"] = "invalid announcement end"
	}
	p.StartsAt = p.StartsAt.UTC()
	if p.EndsAt != nil {
		end := p.EndsAt.UTC()
		p.EndsAt = &end
	}
	if len(fields) > 0 {
		return &validation.Error{Fields: fields}
	}
	return nil
}

type Service struct {
	db         *storage.Database
	now        func() time.Time
	invalidate func()
}

func New(db *storage.Database, now func() time.Time, invalidate func()) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now, invalidate: invalidate}
}

func (s *Service) changed() {
	if s.invalidate != nil {
		s.invalidate()
	}
}

const columns = "id,level,title,body,starts_at,ends_at,created_at,updated_at"

func scan(row interface{ Scan(...any) error }, now time.Time) (Announcement, error) {
	var a Announcement
	var start, created, updated string
	var end sql.NullString
	err := row.Scan(&a.ID, &a.Level, &a.Title, &a.Body, &start, &end, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if a.StartsAt, err = timekey.Parse(start); err != nil {
		return a, err
	}
	if a.CreatedAt, err = timekey.Parse(created); err != nil {
		return a, err
	}
	if a.UpdatedAt, err = timekey.Parse(updated); err != nil {
		return a, err
	}
	if end.Valid {
		t, err := timekey.Parse(end.String)
		if err != nil {
			return a, err
		}
		a.EndsAt = &t
	}
	a.Active = !a.StartsAt.After(now) && (a.EndsAt == nil || a.EndsAt.After(now))
	return a, nil
}

func (s *Service) List(ctx context.Context) ([]Announcement, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	now := s.now().UTC()
	rows, err := lease.QueryContext(ctx, "SELECT "+columns+" FROM announcements WHERE ends_at IS NULL OR ends_at>? ORDER BY starts_at DESC,id", timekey.Format(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Announcement{}
	for rows.Next() {
		a, err := scan(rows, now)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func merge(p *Public, patch Patch, create bool) error {
	for _, f := range []struct {
		name               string
		raw                json.RawMessage
		dst                any
		required, nullable bool
	}{
		{"level", patch.Level, &p.Level, true, false},
		{"title", patch.Title, &p.Title, true, false},
		{"body", patch.Body, &p.Body, true, false},
		{"starts_at", patch.StartsAt, &p.StartsAt, false, false},
		{"ends_at", patch.EndsAt, &p.EndsAt, false, true},
	} {
		if len(f.raw) == 0 {
			if create && f.required {
				return validation.Invalid(f.name, "is required")
			}
			continue
		}
		if !utf8.Valid(f.raw) {
			return validation.Invalid(f.name, "must be valid UTF-8")
		}
		if !f.nullable && strings.TrimSpace(string(f.raw)) == "null" {
			return validation.Invalid(f.name, "cannot be null")
		}
		if json.Unmarshal(f.raw, f.dst) != nil {
			return validation.Invalid(f.name, "has an invalid type")
		}
	}
	return Validate(p)
}

func (s *Service) Save(ctx context.Context, id string, patch Patch) (Announcement, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return Announcement{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	now := s.now().UTC()
	create := id == ""
	a := Announcement{Public: Public{StartsAt: now}, CreatedAt: now, UpdatedAt: now}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	if !create {
		a, err = scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM announcements WHERE id=?", id), now)
		if err != nil {
			return a, err
		}
	}
	if err = merge(&a.Public, patch, create); err != nil {
		return Announcement{}, err
	}
	if create && a.EndsAt != nil && !a.EndsAt.After(now) {
		return Announcement{}, validation.Invalid("ends_at", "invalid announcement end")
	}
	// 重新打开已结束的公告同样受容量限制。
	if a.EndsAt == nil || a.EndsAt.After(now) {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM announcements WHERE id!=? AND (ends_at IS NULL OR ends_at>?)", id, timekey.Format(now)).Scan(&count); err != nil {
			return a, err
		}
		if count >= 100 {
			return Announcement{}, ErrCapacity
		}
	}
	var end any
	if a.EndsAt != nil {
		end = timekey.Format(*a.EndsAt)
	}
	action := "announcement.update"
	a.UpdatedAt = now
	if create {
		id, err = uuid.UUID(rand.Reader)
		if err != nil {
			return a, err
		}
		a.ID = id
		action = "announcement.create"
		_, err = tx.ExecContext(ctx, "INSERT INTO announcements("+columns+") VALUES(?,?,?,?,?,?,?,?)", id, a.Level, a.Title, a.Body, timekey.Format(a.StartsAt), end, timekey.Format(now), timekey.Format(now))
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE announcements SET level=?,title=?,body=?,starts_at=?,ends_at=?,updated_at=? WHERE id=?", a.Level, a.Title, a.Body, timekey.Format(a.StartsAt), end, timekey.Format(now), id)
	}
	if err != nil {
		return Announcement{}, err
	}
	if err = audit.Insert(ctx, tx, action, id, now); err != nil {
		return Announcement{}, err
	}
	if err = tx.Commit(); err != nil {
		return Announcement{}, err
	}
	a.Active = !a.StartsAt.After(now) && (a.EndsAt == nil || a.EndsAt.After(now))
	s.changed()
	return a, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
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
	result, err := tx.ExecContext(ctx, "DELETE FROM announcements WHERE id=?", id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	if err = audit.Insert(ctx, tx, "announcement.delete", id, s.now()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.changed()
	return nil
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
	count, err := storage.DeleteBatch(ctx, tx, `SELECT id FROM announcements WHERE ends_at<? ORDER BY ends_at,id LIMIT ?`, `DELETE FROM announcements WHERE id=?`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, commitErr
	}
	return count, nil
}

// ListPublic 在每页缓存未命中时调用一次，走公开页只读连接池。
func ListPublic(ctx context.Context, db *storage.Database, now time.Time) ([]Public, error) {
	lease, leaseErr := db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	stamp := timekey.Format(now)
	rows, err := lease.QueryContext(ctx, `SELECT level,title,body,starts_at,ends_at FROM announcements
 WHERE starts_at<=? AND (ends_at IS NULL OR ends_at>?)
 ORDER BY CASE level WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,starts_at DESC,id LIMIT 5`, stamp, stamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Public{}
	for rows.Next() {
		var p Public
		var start string
		var end sql.NullString
		if err = rows.Scan(&p.Level, &p.Title, &p.Body, &start, &end); err != nil {
			return nil, err
		}
		if p.StartsAt, err = timekey.Parse(start); err != nil {
			return nil, err
		}
		if end.Valid {
			t, e := timekey.Parse(end.String)
			if e != nil {
				return nil, e
			}
			p.EndsAt = &t
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
