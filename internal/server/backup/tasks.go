package backup

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/timekey"
)

const taskLifetime = 90 * 24 * time.Hour

type TaskPage struct {
	Tasks      []Task  `json:"tasks"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

type PasswordResult struct {
	Password          string    `json:"password"`
	ArchiveName       string    `json:"archive_name"`
	ArchiveSHA256     string    `json:"archive_sha256"`
	PasswordExpiresAt time.Time `json:"password_expires_at"`
}

func taskVisible(task Task, now time.Time) bool {
	return !terminalTask(task) || task.FinishedAt == nil || task.FinishedAt.After(now.Add(-taskLifetime))
}

func terminalTask(task Task) bool { return task.State == "succeeded" || task.State == "failed" }
func passwordEligible(task Task) bool {
	return len(task.passwordCiphertext) != 0 && passwordPublication(task)
}

func passwordPublication(task Task) bool {
	return !task.Restored && (task.State == "succeeded" || task.State == "failed" && task.WarningCode == "storage_outcome_unknown") &&
		task.PasswordExpiresAt != nil && task.ArtifactName != nil &&
		validArtifactName(*task.ArtifactName, task.ID) && task.ArtifactSHA256 != nil && validSHA256(*task.ArtifactSHA256) &&
		task.ResultLocation != nil && *task.ResultLocation != ""
}

func (s *Service) projectTask(task Task, now time.Time) Task {
	task.PasswordAvailable = passwordEligible(task) && now.Before(*task.PasswordExpiresAt)
	task.DownloadAvailable = false
	if artifactEligible(task, now) == nil {
		if file, err := s.openTaskArtifact(task); err == nil {
			task.DownloadAvailable = true
			_ = file.Close()
		}
	}
	return task
}

func (s *Service) Task(ctx context.Context, id string) (Task, error) {
	if !identity.ValidUUID(id) {
		return Task{}, &Error{Code: "task_not_found"}
	}
	task, err := s.loadTask(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, &Error{Code: "task_not_found"}
	}
	if err != nil {
		return Task{}, err
	}
	now := s.opts.Now()
	if !taskVisible(task, now) {
		return Task{}, &Error{Code: "task_not_found"}
	}
	return s.projectTask(task, now), nil
}

func (s *Service) Tasks(ctx context.Context, limit int, before string) (TaskPage, error) {
	page := TaskPage{Tasks: make([]Task, 0)}
	if limit < 1 || limit > 100 {
		return page, &Error{Code: "invalid_input"}
	}
	now := s.opts.Now()
	query := taskSelect + ` WHERE (state NOT IN ('succeeded','failed') OR finished_at IS NULL OR finished_at>?)`
	args := []any{timekey.Format(now.Add(-taskLifetime))}
	if before != "" {
		at, id, err := management.DecodeEventCursor(before)
		if err != nil {
			return page, &Error{Code: "invalid_input"}
		}
		query += ` AND (created_at<? OR (created_at=? AND id<?))`
		args = append(args, at, at, id)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return page, err
		}
		page.Tasks = append(page.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Tasks) > limit {
		page.HasMore = true
		page.Tasks = page.Tasks[:limit]
		last := page.Tasks[limit-1]
		cursor := management.EncodeEventCursor(last.CreatedAt, last.ID)
		page.NextCursor = &cursor
	}
	// Close the read cursor before filesystem checks; no database lease is needed.
	if err := rows.Close(); err != nil {
		return page, err
	}
	for i := range page.Tasks {
		page.Tasks[i] = s.projectTask(page.Tasks[i], now)
	}
	return page, nil
}

// Password is an explicit audited read. Neither Task nor Tasks decrypts secrets.
func (s *Service) Password(ctx context.Context, id string) (PasswordResult, error) {
	var result PasswordResult
	if !identity.ValidUUID(id) {
		return result, &Error{Code: "task_not_found"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return result, &Error{Code: "task_not_found"}
	}
	if err != nil {
		return result, err
	}
	now := s.opts.Now()
	if !taskVisible(task, now) {
		return result, &Error{Code: "task_not_found"}
	}
	if !passwordPublication(task) {
		return result, &Error{Code: "password_unavailable"}
	}
	if !now.Before(*task.PasswordExpiresAt) {
		return result, &Error{Code: "password_expired"}
	}
	if len(task.passwordCiphertext) == 0 {
		return result, &Error{Code: "password_unavailable"}
	}
	if s.box == nil {
		return result, &Error{Code: "secret_unreadable"}
	}
	plain, err := s.box.Open("task-password:"+id, task.passwordCiphertext)
	if err != nil {
		return result, &Error{Code: "secret_unreadable"}
	}
	defer clear(plain)
	if err := audit.Insert(ctx, tx, "backup.password", id, now); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result = PasswordResult{Password: string(plain), ArchiveName: *task.ArtifactName, ArchiveSHA256: *task.ArtifactSHA256, PasswordExpiresAt: *task.PasswordExpiresAt}
	return result, nil
}
