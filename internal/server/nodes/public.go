package nodes

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"modernc.org/sqlite"

	"orbitbeacon/internal/server/audit"
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
	if err := s.db.QueryRowContext(ctx, "SELECT public_settings FROM nodes WHERE id = ?", id).Scan(&raw); err != nil {
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
	tx, err := s.db.BeginTx(ctx, nil)
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

func init() {
	// 在 SQLite 内部用相同的 Unicode 规则校验，确保 LIMIT/OFFSET 之前数据合法。
	sqlite.MustRegisterDeterministicScalarFunction("orbitbeacon_public_valid", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		raw, ok := args[0].(string)
		if ok {
			if _, valid := visibleSettings(raw); valid {
				return int64(1), nil
			}
		}
		return int64(0), nil
	})
}

const publicVisibility = `status = 'unknown' AND token_hash IS NOT NULL AND first_enrolled_at IS NOT NULL
 AND CASE WHEN json_valid(public_settings) THEN
 json_type(public_settings,'$.enabled') = 'true' AND json_type(public_settings,'$.name') = 'text'
	AND length(trim(json_extract(public_settings,'$.name'))) > 0
	AND orbitbeacon_public_valid(public_settings) = 1 ELSE 0 END`

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

// SetPublicReader 选择仅供匿名页面查询使用的隔离连接池。
// 传 nil 恢复为现有构造函数使用的主连接池。
func (s *Service) SetPublicReader(db *sql.DB) { s.publicReader.Store(db) }

func (s *Service) ListPublicRecords(ctx context.Context, limit, offset int) ([]PublicRecord, bool, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, false, ErrInvalidInput
	}
	db := s.publicReader.Load()
	if db == nil {
		db = s.db
	}
	rows, err := db.QueryContext(ctx, "SELECT id,public_settings,last_seen,token_hash,credential_epoch,run_generation,report_session_id FROM nodes WHERE "+publicVisibility+" ORDER BY created_at,id LIMIT ? OFFSET ?", limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	records := make([]PublicRecord, 0, limit)
	for rows.Next() {
		// 多取的一行仅用于判断 has_more。
		if len(records) == limit {
			return records, true, nil
		}
		record, err := scanPublicRecord(rows)
		if err != nil {
			return nil, false, err
		}
		records = append(records, record)
	}
	return records, false, rows.Err()
}

// PublicRecord 是内部身份围栏，绝不作为 HTTP 响应。
type PublicRecord struct {
	ID              string         `json:"-"`
	Settings        PublicSettings `json:"-"`
	LastSeen        time.Time      `json:"-"`
	Credential      [32]byte       `json:"-"`
	CredentialEpoch int64          `json:"-"`
	Generation      uint64         `json:"-"`
	Session         string         `json:"-"`
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
	err := row.Scan(&p.ID, &raw, &seen, &credential, &p.CredentialEpoch, &generation, &p.Session)
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
	if len(credential) != len(p.Credential) {
		return PublicRecord{}, errors.New("invalid public node credential")
	}
	copy(p.Credential[:], credential)
	p.Generation, err = strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return PublicRecord{}, err
	}
	if seen.Valid {
		p.LastSeen, err = timekey.Parse(seen.String)
		if err != nil {
			return PublicRecord{}, err
		}
	}
	return p, nil
}
