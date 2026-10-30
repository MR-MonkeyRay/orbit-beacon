package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

// PlanPatch preserves omitted values. Credentials are an explicit full replacement;
// an omitted session token in a replacement clears the previous token.
type PlanPatch struct {
	Name        *string            `json:"name,omitempty"`
	Enabled     *bool              `json:"enabled,omitempty"`
	Kind        *string            `json:"kind,omitempty"`
	Categories  *[]Category        `json:"categories,omitempty"`
	Cron        *string            `json:"cron,omitempty"`
	Destination json.RawMessage    `json:"destination,omitempty"`
	RetainCount *int               `json:"retain_count,omitempty"`
	Credentials *ObjectCredentials `json:"credentials,omitempty"`
}
type LocalDestination struct {
	Path string `json:"path"`
}
type PlanTaskSummary struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	ErrorCode   string     `json:"error_code"`
	WarningCode string     `json:"warning_code"`
}
type Plan struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Enabled         bool             `json:"enabled"`
	Kind            string           `json:"kind"`
	Categories      []Category       `json:"categories"`
	Cron            string           `json:"cron"`
	Destination     json.RawMessage  `json:"destination"`
	RetainCount     *int             `json:"retain_count"`
	NextRunAt       *time.Time       `json:"next_run_at"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	CredentialsSet  bool             `json:"credentials_set"`
	SessionTokenSet bool             `json:"session_token_set"`
	NextRuns        []NextRun        `json:"next_runs"`
	LastTask        *PlanTaskSummary `json:"last_task"`
}
type PlanPage struct {
	Plans      []Plan  `json:"plans"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
	TimezoneInfo
}
type planRecord struct {
	Plan
	cipher []byte
}

const planSelect = `SELECT id,name,enabled,kind,categories_json,cron,destination_json,credentials_ciphertext,retain_count,next_run_at,created_at,updated_at FROM backup_plans`

func scanPlan(row taskScanner) (planRecord, error) {
	var p planRecord
	var categories, destination, created, updated string
	var next sql.NullString
	err := row.Scan(&p.ID, &p.Name, &p.Enabled, &p.Kind, &categories, &p.Cron, &destination, &p.cipher, &p.RetainCount, &next, &created, &updated)
	if err != nil {
		return p, err
	}
	p.Destination = json.RawMessage(destination)
	if err = json.Unmarshal([]byte(categories), &p.Categories); err != nil {
		return p, err
	}
	if p.CreatedAt, err = timekey.Parse(created); err != nil {
		return p, err
	}
	if p.UpdatedAt, err = timekey.Parse(updated); err != nil {
		return p, err
	}
	if next.Valid {
		at, e := timekey.Parse(next.String)
		if e != nil {
			return p, e
		}
		p.NextRunAt = &at
	}
	return p, nil
}
func planField(field, message string) error {
	return validation.Invalid(field, message)
}
func planMissing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: "backup_plan_not_found"}
	}
	return err
}
func (s *Service) planCredentials(p planRecord) (ObjectCredentials, error) {
	plain, err := s.box.Open("plan:"+p.ID, p.cipher)
	if err != nil {
		return ObjectCredentials{}, &Error{Code: "secret_unreadable"}
	}
	defer clear(plain)
	var c ObjectCredentials
	if decodeStrictObject(plain, &c) != nil || c.Validate() != nil {
		return ObjectCredentials{}, &Error{Code: "secret_unreadable"}
	}
	return c, nil
}
func (s *Service) projectPlan(ctx context.Context, p planRecord, now time.Time) (Plan, error) {
	p.CredentialsSet = len(p.cipher) > 0
	if p.CredentialsSet {
		c, err := s.planCredentials(p)
		if err != nil {
			return Plan{}, err
		}
		p.SessionTokenSet = c.SessionToken != ""
		c = ObjectCredentials{}
	}
	preview, err := s.PreviewCron(p.Cron, now)
	if err != nil {
		return Plan{}, err
	}
	p.NextRuns = preview.NextRuns
	// An enabled plan's persisted due time is authoritative, including an overdue
	// coalesced cycle. Future entries still use the same location-aware helper.
	if p.Enabled && p.NextRunAt != nil {
		p.NextRuns, err = s.nextRuns(p.Cron, *p.NextRunAt, true)
		if err != nil {
			return Plan{}, err
		}
	}
	task, err := scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE plan_id=? AND (state NOT IN ('succeeded','failed') OR finished_at IS NULL OR finished_at>?) ORDER BY created_at DESC,id DESC LIMIT 1`, p.ID, timekey.Format(now.Add(-taskLifetime))))
	if err == nil {
		p.LastTask = &PlanTaskSummary{ID: task.ID, State: task.State, CreatedAt: task.CreatedAt, FinishedAt: task.FinishedAt, ErrorCode: task.ErrorCode, WarningCode: task.WarningCode}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Plan{}, err
	}
	return p.Plan, nil
}
func (s *Service) Plan(ctx context.Context, id string) (Plan, error) {
	if !uuid.ValidUUID(id) {
		return Plan{}, &Error{Code: "backup_plan_not_found"}
	}
	p, err := scanPlan(s.db.QueryRowContext(ctx, planSelect+` WHERE id=?`, id))
	if err != nil {
		return Plan{}, planMissing(err)
	}
	return s.projectPlan(ctx, p, s.opts.Now())
}
func (s *Service) Plans(ctx context.Context, limit int, before string) (PlanPage, error) {
	now := s.opts.Now()
	page := PlanPage{Plans: make([]Plan, 0), TimezoneInfo: s.timezoneInfo(now)}
	if limit < 1 || limit > 100 {
		return page, &Error{Code: "invalid_input"}
	}
	query := planSelect
	args := []any{}
	if before != "" {
		at, id, err := management.DecodeEventCursor(before)
		if err != nil {
			return page, &Error{Code: "invalid_input"}
		}
		query += ` WHERE (created_at<? OR (created_at=? AND id<?))`
		args = append(args, at, at, id)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	var records []planRecord
	for rows.Next() {
		p, e := scanPlan(rows)
		if e != nil {
			rows.Close()
			return page, e
		}
		records = append(records, p)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return page, err
	}
	if len(records) > limit {
		page.HasMore = true
		records = records[:limit]
		last := records[limit-1]
		page.NextCursor = new(management.EncodeEventCursor(last.CreatedAt, last.ID))
	}
	for _, record := range records {
		p, e := s.projectPlan(ctx, record, now)
		if e != nil {
			return page, e
		}
		page.Plans = append(page.Plans, p)
	}
	return page, nil
}
func planBusy(ctx context.Context, tx *sql.Tx, id string) error {
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_tasks WHERE plan_id=? AND state IN ('queued','running'))`, id).Scan(&active); err != nil {
		return err
	}
	if active {
		return &Error{Code: "backup_plan_busy"}
	}
	return nil
}
func mergePlanDestination(old, patch json.RawMessage, kind string, credentials *ObjectCredentials) (json.RawMessage, error) {
	var previous, changes map[string]json.RawMessage
	if len(old) > 0 {
		if err := json.Unmarshal(old, &previous); err != nil {
			return nil, err
		}
	}
	if previous == nil {
		previous = make(map[string]json.RawMessage)
	}
	if len(patch) > 0 {
		if decodeStrictObject(patch, &changes) != nil || changes == nil {
			return nil, planField("destination", "Enter a destination object.")
		}
		for _, v := range changes {
			if string(v) == "null" {
				return nil, planField("destination", "Destination fields cannot be null.")
			}
		}
	}
	var oldProvider, newProvider string
	json.Unmarshal(previous["provider"], &oldProvider)
	json.Unmarshal(changes["provider"], &newProvider)
	providerChanged := oldProvider != "" && newProvider != "" && oldProvider != newProvider
	if providerChanged && (changes["endpoint"] == nil || changes["region"] == nil || credentials == nil) {
		return nil, planField("destination", "Changing provider requires endpoint, region and replacement credentials.")
	}
	for key, value := range changes {
		previous[key] = value
	}
	if kind == "local" {
		raw, _ := json.Marshal(previous)
		var d LocalDestination
		if decodeStrictObject(raw, &d) != nil {
			return nil, planField("destination", "Enter a local path.")
		}
		return json.Marshal(d)
	}
	var provider, region, endpoint string
	json.Unmarshal(previous["provider"], &provider)
	json.Unmarshal(previous["region"], &region)
	json.Unmarshal(previous["endpoint"], &endpoint)
	if provider == "r2" && region == "" {
		region = "auto"
		previous["region"], _ = json.Marshal(region)
	}
	if previous["path_style"] == nil || providerChanged && changes["path_style"] == nil {
		previous["path_style"], _ = json.Marshal(provider == "r2" || provider == "s3_compatible")
	}
	if endpoint == "" && region != "" {
		switch provider {
		case "s3":
			endpoint = "https://s3." + region + ".amazonaws.com"
		case "cos":
			endpoint = "https://cos." + region + ".myqcloud.com"
		case "oss":
			endpoint = "https://s3.oss-" + region + ".aliyuncs.com"
		}
		previous["endpoint"], _ = json.Marshal(endpoint)
	}
	raw, _ := json.Marshal(previous)
	var d ObjectDestination
	if decodeStrictObject(raw, &d) != nil {
		return nil, planField("destination", "Enter a valid object destination.")
	}
	return json.Marshal(d)
}
func validatePlanDirectory(raw json.RawMessage) error {
	var d LocalDestination
	if decodeStrictObject(raw, &d) != nil || securefs.ValidateDirectory(d.Path) != nil {
		return planField("destination.path", "Use an existing owned 0700 directory without symlinks.")
	}
	probe, err := os.CreateTemp(d.Path, ".orbitbeacon-write-probe-")
	if err != nil {
		return planField("destination.path", "The server must be able to write this directory.")
	}
	err = errors.Join(probe.Close(), os.Remove(probe.Name()))
	if err != nil {
		return planField("destination.path", "The server must be able to write and remove files in this directory.")
	}
	return nil
}
func (s *Service) SavePlan(ctx context.Context, id string, patch PlanPatch) (Plan, error) {
	creating := id == ""
	if !creating && !uuid.ValidUUID(id) {
		return Plan{}, &Error{Code: "backup_plan_not_found"}
	}
	if creating {
		s.randomMu.Lock()
		var err error
		id, err = uuid.UUID(s.opts.Random)
		s.randomMu.Unlock()
		if err != nil {
			return Plan{}, &Error{Code: "internal_error"}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, err
	}
	defer tx.Rollback()
	now := s.opts.Now().UTC()
	p := planRecord{Plan: Plan{ID: id, Cron: "0 3 * * *", Categories: []Category{Configuration}, CreatedAt: now}}
	if !creating {
		p, err = scanPlan(tx.QueryRowContext(ctx, planSelect+` WHERE id=?`, id))
		if err != nil {
			return Plan{}, planMissing(err)
		}
		if err = planBusy(ctx, tx, id); err != nil {
			return Plan{}, err
		}
	}
	oldEnabled, oldCron := p.Enabled, p.Cron
	if patch.Kind != nil {
		if !creating && p.Kind != *patch.Kind {
			return Plan{}, planField("kind", "The destination kind cannot be changed.")
		}
		p.Kind = *patch.Kind
	}
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	if patch.Enabled != nil {
		p.Enabled = *patch.Enabled
	}
	if patch.Categories != nil {
		p.Categories = append([]Category(nil), (*patch.Categories)...)
	}
	if patch.Cron != nil {
		p.Cron = *patch.Cron
	}
	if !validText(p.Name, 100) {
		return Plan{}, planField("name", "Enter a name of at most 100 bytes.")
	}
	if !oneOf(p.Kind, "local", "object") {
		return Plan{}, planField("kind", "Choose local or object storage.")
	}
	p.Categories, err = ValidateCategories(p.Categories)
	if err != nil {
		return Plan{}, planField("categories", "Select one or more distinct data categories.")
	}
	if _, err = s.nextRun(p.Cron, now); err != nil {
		return Plan{}, err
	}
	if p.Kind == "local" {
		if creating {
			p.RetainCount = new(7)
		}
		if patch.RetainCount != nil {
			p.RetainCount = new(*patch.RetainCount)
		}
		if p.RetainCount == nil || *p.RetainCount < 1 || *p.RetainCount > 1000 {
			return Plan{}, planField("retain_count", "Keep between 1 and 1000 archives.")
		}
		if patch.Credentials != nil {
			return Plan{}, planField("credentials", "Local plans do not use credentials.")
		}
	} else if patch.RetainCount != nil {
		return Plan{}, planField("retain_count", "Object retention is managed by the storage provider.")
	}
	p.Destination, err = mergePlanDestination(p.Destination, patch.Destination, p.Kind, patch.Credentials)
	if err != nil {
		return Plan{}, err
	}
	definition := PlanDefinition{Name: p.Name, Enabled: p.Enabled, Kind: p.Kind, Categories: p.Categories, Cron: p.Cron, Destination: p.Destination, RetainCount: p.RetainCount}
	if err = ValidatePlan(definition); err != nil {
		return Plan{}, planField("destination", "Enter a valid destination for this provider.")
	}
	if p.Kind == "local" {
		if err = validatePlanDirectory(p.Destination); err != nil {
			return Plan{}, err
		}
	} else {
		if patch.Credentials != nil {
			c := *patch.Credentials
			if c.Validate() != nil || maskedCredential(c.AccessKeyID) || maskedCredential(c.SecretAccessKey) || c.SessionToken != "" && maskedCredential(c.SessionToken) {
				return Plan{}, planField("credentials", "Provide a complete, unmasked access key and secret.")
			}
			plain, e := json.Marshal(c)
			c = ObjectCredentials{}
			if e != nil {
				return Plan{}, &Error{Code: "internal_error"}
			}
			p.cipher, err = s.box.Seal("plan:"+id, plain)
			clear(plain)
			if err != nil {
				return Plan{}, &Error{Code: "secret_unreadable"}
			}
		}
		if len(p.cipher) == 0 {
			return Plan{}, planField("credentials", "Provide an access key and secret.")
		}
		if _, e := s.planCredentials(p); e != nil {
			return Plan{}, e
		}
	}
	if !p.Enabled {
		p.NextRunAt = nil
	} else if creating || !oldEnabled || oldCron != p.Cron {
		next, e := s.nextRun(p.Cron, now)
		if e != nil {
			return Plan{}, e
		}
		p.NextRunAt = &next
	}
	p.UpdatedAt = now
	cats, _ := json.Marshal(p.Categories)
	var next any
	if p.NextRunAt != nil {
		next = timekey.Format(*p.NextRunAt)
	}
	if creating {
		_, err = tx.ExecContext(ctx, `INSERT INTO backup_plans(id,name,enabled,kind,categories_json,cron,destination_json,credentials_ciphertext,retain_count,next_run_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, p.Name, p.Enabled, p.Kind, string(cats), p.Cron, string(p.Destination), p.cipher, p.RetainCount, next, timekey.Format(p.CreatedAt), timekey.Format(now))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE backup_plans SET name=?,enabled=?,categories_json=?,cron=?,destination_json=?,credentials_ciphertext=?,retain_count=?,next_run_at=?,updated_at=? WHERE id=?`, p.Name, p.Enabled, string(cats), p.Cron, string(p.Destination), p.cipher, p.RetainCount, next, timekey.Format(now), id)
	}
	if err != nil {
		return Plan{}, err
	}
	action := "backup.plan.update"
	if creating {
		action = "backup.plan.create"
	}
	if err = audit.InsertDetails(ctx, tx, action, id, now, audit.Details{"categories": p.Categories, "enabled": p.Enabled}); err != nil {
		return Plan{}, err
	}
	if err = tx.Commit(); err != nil {
		return Plan{}, err
	}
	s.signal()
	return s.projectPlan(ctx, p, now)
}
func maskedCredential(value string) bool { return strings.Trim(value, "*• ") == "" }
func (s *Service) DeletePlan(ctx context.Context, id string) error {
	if !uuid.ValidUUID(id) {
		return &Error{Code: "backup_plan_not_found"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_plans WHERE id=?)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return &Error{Code: "backup_plan_not_found"}
	}
	if err = planBusy(ctx, tx, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM backup_plans WHERE id=?`, id); err != nil {
		return err
	}
	if err = audit.InsertDetails(ctx, tx, "backup.plan.delete", id, s.opts.Now().UTC(), nil); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.signal()
	return nil
}
