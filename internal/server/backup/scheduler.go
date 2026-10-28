package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/timekey"
)

type TimezoneInfo struct {
	TimezoneName     string    `json:"timezone_name"`
	UTCOffsetSeconds int       `json:"utc_offset_seconds"`
	ServerTime       time.Time `json:"server_time"`
}
type NextRun struct {
	At    time.Time `json:"at"`
	Local string    `json:"local"`
}
type CronPreview struct {
	NextRuns []NextRun `json:"next_runs"`
	TimezoneInfo
}

func (s *Service) timezoneInfo(now time.Time) TimezoneInfo {
	local := now.In(s.serverLocation)
	zone, offset := local.Zone()
	name := s.serverLocation.String()
	// "Local" is Go's process-local sentinel, not an IANA identifier. The actual
	// current zone abbreviation is an honest display label in that case.
	if name == "Local" || name == "" {
		name = zone
	}
	if name == "" {
		name = "Server local time"
	}
	return TimezoneInfo{TimezoneName: name, UTCOffsetSeconds: offset, ServerTime: now.UTC()}
}
func (s *Service) nextRun(raw string, now time.Time) (time.Time, error) {
	schedule, err := ParseSchedule(raw)
	if err != nil {
		return time.Time{}, planField("cron", "Enter a valid five-field Cron expression.")
	}
	next := schedule.Next(now.In(s.serverLocation))
	if next.IsZero() {
		return time.Time{}, planField("cron", "This expression has no upcoming run.")
	}
	return next.UTC(), nil
}
func (s *Service) nextRuns(raw string, now time.Time, include bool) ([]NextRun, error) {
	runs := make([]NextRun, 0, 3)
	at := now.UTC()
	for range 3 {
		if !include {
			var err error
			at, err = s.nextRun(raw, at)
			if err != nil {
				return nil, err
			}
		}
		include = false
		runs = append(runs, NextRun{At: at, Local: at.In(s.serverLocation).Format(time.RFC3339)})
	}
	return runs, nil
}
func (s *Service) PreviewCron(raw string, now time.Time) (CronPreview, error) {
	result := CronPreview{TimezoneInfo: s.timezoneInfo(now)}
	var err error
	result.NextRuns, err = s.nextRuns(raw, now, false)
	return result, err
}

// Tick atomically coalesces each due plan into at most one task. Blocked plans
// retain their original due timestamp; the merged wake retries them on completion.
func (s *Service) Tick(ctx context.Context, now time.Time) error {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM backup_tasks WHERE state IN ('queued','running')`).Scan(&active); err != nil {
		return err
	}
	if active >= 32 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, planSelect+` WHERE enabled=1 AND next_run_at IS NOT NULL AND next_run_at<=? ORDER BY next_run_at,id`, timekey.Format(now))
	if err != nil {
		return err
	}
	var due []planRecord
	for rows.Next() {
		p, e := scanPlan(rows)
		if e != nil {
			rows.Close()
			return e
		}
		due = append(due, p)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	enqueued := false
	for _, p := range due {
		if active >= 32 {
			break
		}
		if err = planBusy(ctx, tx, p.ID); err != nil {
			var coded *Error
			if errors.As(err, &coded) && coded.Code == "backup_plan_busy" {
				continue
			}
			return err
		}
		next, e := s.nextRun(p.Cron, now)
		if e != nil {
			return e
		}
		s.randomMu.Lock()
		id, e := identity.UUID(s.opts.Random)
		s.randomMu.Unlock()
		if e != nil {
			return &Error{Code: "internal_error"}
		}
		destination := TaskDestination{PlanName: p.Name, Target: p.Destination}
		kind := "backup_remote"
		var cipher []byte
		if p.Kind == "local" {
			kind = "backup_local"
			var d LocalDestination
			if decodeStrictObject(p.Destination, &d) != nil || p.RetainCount == nil {
				return &Error{Code: "internal_error"}
			}
			destination.Target, e = json.Marshal(struct {
				Path        string `json:"path"`
				RetainCount int    `json:"retain_count"`
			}{d.Path, *p.RetainCount})
			if e != nil {
				return e
			}
		} else if p.Kind == "object" {
			credentials, e := s.planCredentials(p)
			if e != nil {
				return e
			}
			plain, e := json.Marshal(credentials)
			credentials = ObjectCredentials{}
			if e != nil {
				return &Error{Code: "internal_error"}
			}
			cipher, e = s.box.Seal("task-input:"+id, plain)
			clear(plain)
			if e != nil {
				return &Error{Code: "secret_unreadable"}
			}
		} else {
			return &Error{Code: "internal_error"}
		}
		target, e := json.Marshal(destination)
		if e != nil {
			return e
		}
		cats, e := json.Marshal(p.Categories)
		if e != nil {
			return e
		}
		// The uniqueness constraint is a second durable barrier against replaying a
		// scheduled UTC instant, including repeated local wall times during DST.
		result, e := tx.ExecContext(ctx, `INSERT INTO backup_tasks(id,kind,state,plan_id,categories_json,destination_json,input_credentials_ciphertext,scheduled_at,created_at) VALUES(?,?,'queued',?,?,?,?,?,?) ON CONFLICT(plan_id,scheduled_at) DO NOTHING`, id, kind, p.ID, string(cats), string(target), cipher, timekey.Format(*p.NextRunAt), timekey.Format(now))
		if e != nil {
			return e
		}
		inserted, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if inserted == 1 {
			active++
			enqueued = true
			if e = audit.InsertDetails(ctx, tx, "backup.schedule", id, now, audit.Details{"plan_id": p.ID, "categories": p.Categories, "state": "queued"}); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(ctx, `UPDATE backup_plans SET next_run_at=? WHERE id=?`, timekey.Format(next), p.ID); e != nil {
			return e
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if enqueued {
		s.signal()
	}
	return nil
}

// Past due plans can be blocked by capacity or their own active task. Never
// reset a timer to their past timestamp: completion/mutation wakes them, while
// this bounded fallback also covers transient database failures.
func (s *Service) schedulerWait(ctx context.Context, now time.Time) (time.Duration, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT next_run_at FROM backup_plans WHERE enabled=1 AND next_run_at>? ORDER BY next_run_at,id LIMIT 1`, timekey.Format(now)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Minute, nil
	}
	if err != nil {
		return time.Second, err
	}
	at, err := timekey.Parse(raw)
	if err != nil {
		return time.Second, err
	}
	wait := at.Sub(now)
	if wait > time.Minute {
		wait = time.Minute
	}
	if wait <= 0 {
		wait = time.Second
	}
	return wait, nil
}
