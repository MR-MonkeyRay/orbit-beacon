package nodes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

// OnlineWindow 是管理端和公开端共用的近期上报窗口。
const OnlineWindow = 180 * time.Second

// PublicFields defines the storage and wire order for explicit disclosure.
var PublicFields = []string{"region", "provider", "cpu", "memory", "load", "uptime", "availability", "traffic", "expires_on", "price", "billing_cycle", "bandwidth", "ip_version", "network_route", "plan_extra"}
var DefaultPublicFields = []string{"cpu", "memory", "load", "uptime", "availability"}

type PublicSettings struct {
	Enabled bool     `json:"enabled"`
	Fields  []string `json:"fields"`
}

func (p *PublicSettings) UnmarshalJSON(data []byte) error {
	type wire PublicSettings
	value := wire{Fields: append([]string{}, DefaultPublicFields...)}
	if !utf8.Valid(data) {
		return validation.Invalid("fields", "must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Fields == nil {
		return validation.Invalid("fields", "must be an array")
	}
	*p = PublicSettings(value)
	return p.Validate()
}

type PublicSettingsPatch struct {
	Enabled *bool     `json:"enabled"`
	Fields  *[]string `json:"fields"`
}

func (p *PublicSettingsPatch) UnmarshalJSON(data []byte) error {
	if !utf8.Valid(data) {
		return validation.Invalid("fields", "must be valid UTF-8")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range []string{"enabled", "fields"} {
		if bytes.Equal(bytes.TrimSpace(raw[key]), []byte("null")) {
			return validation.Invalid(key, "must not be null")
		}
	}
	type wire PublicSettingsPatch
	var w wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w); err != nil {
		return err
	}
	*p = PublicSettingsPatch(w)
	return nil
}
func (p PublicSettings) Validate() error {
	seen := map[string]bool{}
	for _, field := range p.Fields {
		if !slices.Contains(PublicFields, field) {
			return validation.Invalid("fields", "unknown field")
		}
		if seen[field] {
			return validation.Invalid("fields", "duplicate field")
		}
		seen[field] = true
	}
	return nil
}
func canonicalFields(fields []string) []string {
	out := make([]string, 0, len(fields))
	for _, field := range PublicFields {
		if slices.Contains(fields, field) {
			out = append(out, field)
		}
	}
	return out
}
func decodePublic(raw string) (PublicSettings, error) {
	p := PublicSettings{Fields: append([]string{}, DefaultPublicFields...)}
	if !utf8.ValidString(raw) {
		return p, errors.New("invalid public settings UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, err
	}
	if p.Fields == nil {
		return p, validation.Invalid("fields", "must be an array")
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	p.Fields = canonicalFields(p.Fields)
	return p, nil
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
	oldFields := append([]string{}, p.Fields...)
	if patch.Enabled != nil {
		p.Enabled = *patch.Enabled
	}
	if patch.Fields != nil {
		p.Fields = append([]string{}, (*patch.Fields)...)
	}
	if err := p.Validate(); err != nil {
		return PublicSettings{}, err
	}
	p.Fields = canonicalFields(p.Fields)
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
	withdrawn := wasEnabled && !p.Enabled
	for _, field := range oldFields {
		if wasEnabled && !slices.Contains(p.Fields, field) {
			withdrawn = true
		}
	}
	if withdrawn {
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
	records, more, _, err := s.ListPublicRecordsWithGroups(ctx, limit, offset, now, false)
	return records, more, err
}

// ListPublicRecordsWithGroups derives group membership from the same validated
// candidate scan as pagination, over the entire public inventory when requested.
func (s *Service) ListPublicRecordsWithGroups(ctx context.Context, limit, offset int, now time.Time, showGroups bool) ([]PublicRecord, bool, []string, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, false, nil, ErrInvalidInput
	}
	lease, err := s.db.Acquire(ctx, storage.Reader)
	if err != nil {
		return nil, false, nil, err
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginRead(ctx)
	if err != nil {
		return nil, false, nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,public_settings,last_seen,token_hash,credential_epoch,run_generation,report_session_id,'[]',name,region,provider,geo_country,geo_location,expires_on,price_amount,price_currency,billing_cycle,auto_renew,bandwidth,ipv4,ipv6,network_route,plan_extra,quota_bytes,group_id FROM nodes WHERE status='unknown' AND token_hash IS NOT NULL AND first_enrolled_at IS NOT NULL ORDER BY display_order DESC,name,id`)
	if err != nil {
		return nil, false, nil, err
	}
	records := make([]PublicRecord, 0, limit)
	more := false
	visibleGroups := map[string]bool{}
	for rows.Next() {
		record, scanErr := scanPublicRecord(rows)
		if errors.Is(scanErr, ErrNotFound) {
			continue
		}
		if scanErr != nil {
			rows.Close()
			return nil, false, nil, scanErr
		}
		if showGroups && record.GroupID != nil {
			visibleGroups[*record.GroupID] = true
		}
		if offset > 0 {
			offset--
			continue
		}
		if len(records) == limit {
			more = true
			if showGroups {
				continue
			}
			break
		}
		records = append(records, record)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, nil, err
	}
	var groupNames []string
	if showGroups {
		groupNames = []string{}
		indexes := map[string]int{}
		rows, err = tx.QueryContext(ctx, "SELECT id,name FROM `groups` ORDER BY display_order DESC,name,id")
		if err != nil {
			return nil, false, nil, err
		}
		for rows.Next() {
			var id, name string
			if err = rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, false, nil, err
			}
			if visibleGroups[id] {
				indexes[id] = len(groupNames)
				groupNames = append(groupNames, name)
			}
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, false, nil, err
		}
		for i := range records {
			if records[i].GroupID != nil {
				if index, ok := indexes[*records[i].GroupID]; ok {
					records[i].Group = &index
				}
			}
		}
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
			return nil, false, nil, err
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
				return nil, false, nil, err
			}
			i := byID[id]
			counts[i] = append(counts[i], day)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, false, nil, err
		}
		for i, days := range counts {
			if len(days) > 0 {
				raw, err := json.Marshal(days)
				if err != nil {
					return nil, false, nil, err
				}
				records[i].AvailabilityJSON = string(raw)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, false, nil, err
	}
	return records, more, groupNames, nil
}

// PublicRecord 是内部身份围栏，绝不作为 HTTP 响应。
type PublicRecord struct {
	Name, Region, Provider, Country, GeoLocation                                             string
	ExpiresOn                                                                                *string
	PriceAmount, PriceCurrency, BillingCycle, Bandwidth, NetworkRoute, PlanExtra, QuotaBytes string
	AutoRenew, IPv4, IPv6                                                                    bool

	GroupID          *string        `json:"-"`
	Group            *int           `json:"-"`
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
	err := row.Scan(&p.ID, &raw, &seen, &credential, &p.CredentialEpoch, &generation, &p.Session, &p.AvailabilityJSON, &p.Name, &p.Region, &p.Provider, &p.Country, &p.GeoLocation, &p.ExpiresOn, &p.PriceAmount, &p.PriceCurrency, &p.BillingCycle, &p.AutoRenew, &p.Bandwidth, &p.IPv4, &p.IPv6, &p.NetworkRoute, &p.PlanExtra, &p.QuotaBytes, &p.GroupID)
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
