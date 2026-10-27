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

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/timekey"
)

// PublicSettings contains only text explicitly selected for anonymous publication.
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

// Preserve raw text validity: encoding/json otherwise silently replaces invalid
// UTF-8 with U+FFFD before the service can validate the administrator's input.
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
			return &ValidationError{Fields: map[string]string{field.name: "must be valid UTF-8"}}
		}
		if len(field.raw) != 0 {
			if err := json.Unmarshal(field.raw, field.dst); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p PublicSettings) validate() error {
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
		return &ValidationError{Fields: fields}
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
	if !identity.ValidUUID(id) {
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
	if patch.Enabled != nil {
		p.Enabled = *patch.Enabled
	}
	for _, field := range []struct {
		dst *string
		src *string
	}{{&p.Name, patch.Name}, {&p.Region, patch.Region}, {&p.Provider, patch.Provider}} {
		if field.src != nil {
			// Validate before trimming so whitespace control characters cannot disappear.
			if !utf8.ValidString(*field.src) || strings.ContainsFunc(*field.src, unicode.IsControl) {
				*field.dst = *field.src
			} else {
				*field.dst = strings.TrimSpace(*field.src)
			}
		}
	}
	if err := p.validate(); err != nil {
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
	return p, nil
}

func init() {
	// Validate inside SQLite before LIMIT/OFFSET with identical Unicode rules.
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
	return p, err == nil && p.Enabled && p.validate() == nil
}

func (s *Service) ListPublicIDs(ctx context.Context, limit, offset int) ([]string, bool, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, false, ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM nodes WHERE "+publicVisibility+" ORDER BY created_at,id LIMIT ? OFFSET ?", limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		if len(ids) == limit {
			return ids, true, nil
		}
		ids = append(ids, id)
	}
	return ids, false, rows.Err()
}

// PublicRecord is an internal identity fence, never an HTTP representation.
type PublicRecord struct {
	Settings   PublicSettings `json:"-"`
	LastSeen   time.Time      `json:"-"`
	Credential [32]byte       `json:"-"`
	Generation uint64         `json:"-"`
	Session    string         `json:"-"`
}

func ReadPublicRecord(ctx context.Context, db *sql.DB, id string) (PublicRecord, error) {
	var p PublicRecord
	var raw, generation string
	var seen sql.NullString
	var credential []byte
	err := db.QueryRowContext(ctx, "SELECT public_settings,last_seen,token_hash,run_generation,report_session_id FROM nodes WHERE id = ? AND "+publicVisibility, id).Scan(&raw, &seen, &credential, &generation, &p.Session)
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
