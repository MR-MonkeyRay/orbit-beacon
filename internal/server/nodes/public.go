package nodes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"orbitbeacon/internal/server/announcements"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

// OnlineWindow 是管理端和公开端共用的近期上报窗口。
const OnlineWindow = 180 * time.Second

// PublicSettings 仅包含明确选择用于匿名公开的文本。
type PublicSettings struct {
	Enabled  bool   `json:"enabled"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	Provider string `json:"provider"`
}

type PublicSettingsPatch struct {
	Enabled  *bool   `json:"enabled"`
	Name     *string `json:"name"`
	Region   *string `json:"region"`
	Provider *string `json:"provider"`
}

// 保留原始文本的有效性：否则 encoding/json 会在服务校验管理员输入之前，
// 悄悄把非法 UTF-8 替换为 U+FFFD。
func (p *PublicSettingsPatch) UnmarshalJSON(data []byte) error {
	var wire struct {
		Enabled  *bool           `json:"enabled"`
		Name     json.RawMessage `json:"name"`
		Region   json.RawMessage `json:"region"`
		Provider json.RawMessage `json:"provider"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*p = PublicSettingsPatch{Enabled: wire.Enabled}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
		dst  **string
	}{
		{"name", wire.Name, &p.Name}, {"region", wire.Region, &p.Region}, {"provider", wire.Provider, &p.Provider},
	} {
		if !utf8.Valid(field.raw) {
			return validation.Invalid(field.name, "must be valid UTF-8")
		}
		if len(field.raw) != 0 {
			if err := json.Unmarshal(field.raw, field.dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate 校验公开字段，不改动已存储的设置。
func (p PublicSettings) Validate() error {
	fields := make(map[string]string)
	for _, field := range []struct {
		name, value string
		max         int
	}{{"name", p.Name, 255}, {"region", p.Region, 128}, {"provider", p.Provider, 128}} {
		if !utf8.ValidString(field.value) || len(field.value) > field.max || strings.ContainsFunc(field.value, unicode.IsControl) {
			fields[field.name] = fmt.Sprintf("must be valid UTF-8 without control characters and at most %d bytes", field.max)
		}
	}
	if p.Enabled && strings.TrimSpace(p.Name) == "" {
		fields["name"] = "must be nonempty when published"
	}
	if len(fields) > 0 {
		return &validation.Error{Fields: fields}
	}
	return nil
}

func decodePublic(raw string) (PublicSettings, error) {
	var p PublicSettings
	if !utf8.ValidString(raw) {
		return p, errors.New("invalid public settings UTF-8")
	}
	err := json.Unmarshal([]byte(raw), &p)
	return p, err
}

func (s *Service) GetPublicSettings(ctx context.Context, id string) (PublicSettings, error) {
	var raw string
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return PublicSettings{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if err := lease.QueryRowContext(ctx, "SELECT public_settings FROM nodes WHERE id = ?", id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublicSettings{}, ErrNotFound
		}
		return PublicSettings{}, err
	}
	return decodePublic(raw)
}

func (s *Service) PatchPublicSettings(ctx context.Context, id string, patch PublicSettingsPatch) (PublicSettings, error) {
	if !uuid.ValidUUID(id) {
		return PublicSettings{}, ErrInvalidInput
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return PublicSettings{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return PublicSettings{}, err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT public_settings FROM nodes WHERE id = ?", id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublicSettings{}, ErrNotFound
		}
		return PublicSettings{}, err
	}
	p, err := decodePublic(raw)
	if err != nil {
		return PublicSettings{}, err
	}
	wasEnabled := p.Enabled
	if patch.Enabled != nil {
		p.Enabled = *patch.Enabled
	}
	for _, field := range []struct {
		dst *string
		src *string
	}{{&p.Name, patch.Name}, {&p.Region, patch.Region}, {&p.Provider, patch.Provider}} {
		if field.src != nil {
			// 先校验再修剪，这样空白中的控制字符不会消失。
			if !utf8.ValidString(*field.src) || strings.ContainsFunc(*field.src, unicode.IsControl) {
				*field.dst = *field.src
			} else {
				*field.dst = strings.TrimSpace(*field.src)
			}
		}
	}
	if err := p.Validate(); err != nil {
		return PublicSettings{}, err
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return PublicSettings{}, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE nodes SET public_settings = ? WHERE id = ?", string(encoded), id)
	if err := affected(result, err); err != nil {
		return PublicSettings{}, err
	}
	if err := audit.Insert(ctx, tx, "node.public_settings.patch", id, s.now()); err != nil {
		return PublicSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublicSettings{}, err
	}
	if wasEnabled && !p.Enabled {
		s.InvalidatePublicSafety()
	} else {
		s.InvalidatePublic()
	}
	return p, nil
}

func visibleSettings(raw string) (PublicSettings, bool) {
	p, err := decodePublic(raw)
	return p, err == nil && p.Enabled && p.Validate() == nil
}

// PublicVisibilityVersion 在公开数据变更提交后递增。
func (s *Service) PublicVisibilityVersion() uint64 {
	s.publicMu.RLock()
	defer s.publicMu.RUnlock()
	return s.publicVersion
}

// ListPublicRecords 在同一快照中过滤合法候选，再批量读取所选节点的可用性。
func (s *Service) ListPublicRecords(ctx context.Context, limit, offset int, now time.Time) ([]PublicRecord, bool, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, false, ErrInvalidInput
	}
	lease, err := s.db.Acquire(ctx, storage.Reader)
	if err != nil {
		return nil, false, err
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginRead(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,public_settings,last_seen,token_hash,credential_epoch,run_generation,report_session_id,'[]' FROM nodes WHERE status='unknown' AND token_hash IS NOT NULL AND first_enrolled_at IS NOT NULL ORDER BY created_at,id`)
	if err != nil {
		return nil, false, err
	}
	records := make([]PublicRecord, 0, limit)
	more := false
	for rows.Next() {
		record, scanErr := scanPublicRecord(rows)
		if errors.Is(scanErr, ErrNotFound) {
			continue
		}
		if scanErr != nil {
			rows.Close()
			return nil, false, scanErr
		}
		if offset > 0 {
			offset--
			continue
		}
		if len(records) == limit {
			more = true
			break
		}
		records = append(records, record)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if len(records) > 0 {
		args := make([]any, 0, len(records)+2)
		byID := make(map[string]int, len(records))
		for i, record := range records {
			args = append(args, record.ID)
			byID[record.ID] = i
		}
		now = now.UTC()
		args = append(args, now.AddDate(0, 0, -29).Format(time.DateOnly), now.Format(time.DateOnly))
		rows, err = tx.QueryContext(ctx, `SELECT node_id,day,online_seconds,offline_seconds FROM node_availability_daily WHERE node_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(records)), ",")+`) AND day>=? AND day<=? ORDER BY node_id,day`, args...)
		if err != nil {
			return nil, false, err
		}
		type daily struct {
			Day     string `json:"day"`
			Online  int64  `json:"online_seconds"`
			Offline int64  `json:"offline_seconds"`
		}
		counts := make([][]daily, len(records))
		for rows.Next() {
			var id string
			var day daily
			if err = rows.Scan(&id, &day.Day, &day.Online, &day.Offline); err != nil {
				rows.Close()
				return nil, false, err
			}
			i := byID[id]
			counts[i] = append(counts[i], day)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, false, err
		}
		for i, days := range counts {
			if len(days) > 0 {
				raw, err := json.Marshal(days)
				if err != nil {
					return nil, false, err
				}
				records[i].AvailabilityJSON = string(raw)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return records, more, nil
}

// PublicRecord 是内部身份围栏，绝不作为 HTTP 响应。
type PublicRecord struct {
	ID               string         `json:"-"`
	AvailabilityJSON string         `json:"-"`
	Settings         PublicSettings `json:"-"`
	LastSeen         time.Time      `json:"-"`
	Credential       [32]byte       `json:"-"`
	CredentialEpoch  int64          `json:"-"`
	Generation       uint64         `json:"-"`
	Session          string         `json:"-"`
}

// LatestSeen 包含持久化 last_seen 之后在内存中合并的上报。
// 调用方必须先匹配该记录的凭据和进程身份。
func (p PublicRecord) LatestSeen(receivedAt time.Time) time.Time {
	if receivedAt.After(p.LastSeen) {
		return receivedAt
	}
	return p.LastSeen
}

func scanPublicRecord(row interface{ Scan(...any) error }) (PublicRecord, error) {
	var p PublicRecord
	var raw, generation string
	var seen sql.NullString
	var credential []byte
	err := row.Scan(&p.ID, &raw, &seen, &credential, &p.CredentialEpoch, &generation, &p.Session, &p.AvailabilityJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	var ok bool
	p.Settings, ok = visibleSettings(raw)
	if !ok {
		return PublicRecord{}, ErrNotFound
	}
	if len(credential) != len(p.Credential) || p.CredentialEpoch < 0 {
		return PublicRecord{}, ErrNotFound
	}
	copy(p.Credential[:], credential)
	p.Generation, err = strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return PublicRecord{}, ErrNotFound
	}
	if seen.Valid {
		p.LastSeen, err = timekey.Parse(seen.String)
		if err != nil {
			return PublicRecord{}, ErrNotFound
		}
	}
	return p, nil
}

func (s *Service) ListPublicAnnouncements(ctx context.Context, now time.Time) ([]announcements.Public, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	return announcements.ListPublic(ctx, s.db, now)
}
