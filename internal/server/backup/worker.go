package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

// Run owns the one execution slot. Only its service context controls accepted work.
func (s *Service) Run(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	defer s.running.Store(false)
	defer s.closeCleanup()
	for ctx.Err() == nil {
		if err := s.recoverRunning(ctx); err == nil {
			break
		}
		if !workerDelay(ctx) {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	_ = s.cleanupWork(ctx)
	_, _ = s.CleanupOrphans(ctx, 500)
	_, _ = s.CleanupLocalTemps(ctx, 500)
	for ctx.Err() == nil {
		task, err := s.claim(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(time.Minute):
			}
			continue
		}
		if err != nil {
			if !workerDelay(ctx) {
				return
			}
			continue
		}
		s.execute(ctx, task)
	}
}
func workerDelay(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (s *Service) recoverRunning(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, taskSelect+` WHERE state='running' ORDER BY created_at,id`)
	if err != nil {
		return err
	}
	var tasks []Task
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			rows.Close()
			return e
		}
		tasks = append(tasks, t)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, task := range tasks {
		warning := ""
		if task.Phase == "storing" {
			warning = "storage_outcome_unknown"
		}
		if !s.finish(ctx, task, "failed", "interrupted", warning) {
			return ctx.Err()
		}
	}
	return nil
}
func (s *Service) claim(ctx context.Context) (Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE state='queued' ORDER BY created_at,id LIMIT 1`))
	if err != nil {
		return Task{}, err
	}
	if !identity.ValidUUID(task.ID) {
		return Task{}, &Error{Code: "internal_error"}
	}
	now := s.opts.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE backup_tasks SET state='running',phase='snapshot',started_at=? WHERE id=? AND state='queued'`, timekey.Format(now), task.ID)
	if err != nil {
		return Task{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Task{}, err
	}
	if n != 1 {
		return Task{}, sql.ErrNoRows
	}
	if err = tx.Commit(); err != nil {
		return Task{}, err
	}
	task.State = "running"
	task.Phase = "snapshot"
	task.StartedAt = &now
	return task, nil
}
func privateDirectory(path string) error {
	if err := securefs.ValidateParents(path); err != nil {
		return err
	}
	err := os.Mkdir(path, 0700)
	if err != nil && !os.IsExist(err) {
		return err
	}
	if check := securefs.ValidateDirectory(path); check != nil {
		return check
	}
	if err == nil {
		return syncWorkerDirectory(filepath.Dir(path))
	}
	return nil
}
func syncWorkerDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (s *Service) execute(ctx context.Context, task Task) {
	warning := ""
	fail := func(err error, fallback string) {
		if ctx.Err() == nil {
			s.finish(ctx, task, "failed", safeWorkerCode(err, fallback), warning)
		}
	}
	s.randomMu.Lock()
	password, err := GeneratePassword(s.opts.Random)
	s.randomMu.Unlock()
	defer func() { password = "" }()
	if err != nil {
		fail(err, "internal_error")
		return
	}
	plain := []byte(password)
	cipher, err := s.box.Seal("task-password:"+task.ID, plain)
	clear(plain)
	if err != nil {
		fail(err, "secret_unreadable")
		return
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE backup_tasks SET password_ciphertext=?,password_expires_at=NULL WHERE id=? AND state='running'`, cipher, task.ID); err != nil {
		fail(err, "internal_error")
		return
	}
	task.passwordCiphertext = cipher
	root := filepath.Join(s.opts.DataDir, "backup-work")
	if err = privateDirectory(root); err != nil {
		fail(err, "snapshot_failed")
		return
	}
	work := filepath.Join(root, task.ID)
	if err = os.Mkdir(work, 0700); err != nil {
		fail(err, "snapshot_failed")
		return
	}
	if err = securefs.ValidateDirectory(work); err != nil {
		fail(err, "snapshot_failed")
		return
	}
	defer os.RemoveAll(work)
	payloads, err := CreateSnapshotPayloads(ctx, s.opts.DataDir, work, task.Categories, s.opts.Now)
	if err != nil {
		fail(err, "snapshot_failed")
		return
	}
	created := s.opts.Now().UTC()
	name := "orbitbeacon-" + created.Format("20060102T150405Z") + "-" + task.ID + ".tar.gz.age"
	if _, err = s.db.ExecContext(ctx, `UPDATE backup_tasks SET phase='packaging',artifact_name=? WHERE id=? AND state='running'`, name, task.ID); err != nil {
		fail(err, "internal_error")
		return
	}
	task.Phase = "packaging"
	task.ArtifactName = &name
	outputDir := work
	var local struct {
		Path        string `json:"path"`
		RetainCount int    `json:"retain_count"`
	}
	var destination ObjectDestination
	var credentials ObjectCredentials
	var objectKey string
	switch task.Kind {
	case "backup_export":
		exports := filepath.Join(s.opts.DataDir, "exports")
		if err = privateDirectory(exports); err == nil {
			outputDir = filepath.Join(exports, task.ID)
			err = privateDirectory(outputDir)
		}
	case "backup_local":
		if err = decodeStrictObject(task.Destination.Target, &local); err == nil {
			if task.PlanID == nil || !identity.ValidUUID(*task.PlanID) || !filepath.IsAbs(local.Path) || local.RetainCount < 1 || local.RetainCount > 1000 {
				err = &Error{Code: "internal_error"}
			} else if err = securefs.ValidateDirectory(local.Path); err == nil {
				outputDir = filepath.Join(local.Path, "orbitbeacon-"+*task.PlanID)
				err = privateDirectory(outputDir)
			}
		}
	case "backup_remote":
		if err = decodeStrictObject(task.Destination.Target, &destination); err == nil {
			err = destination.Validate()
		}
		if err == nil {
			var raw []byte
			raw, err = s.box.Open("task-input:"+task.ID, task.inputCredentialsCiphertext)
			if err != nil {
				err = &Error{Code: "secret_unreadable", Err: err}
			} else {
				err = decodeStrictObject(raw, &credentials)
				clear(raw)
				if err == nil {
					err = credentials.Validate()
				}
			}
		}
		if err == nil && s.opts.Uploader == nil {
			err = &Error{Code: "storage_upload_failed"}
		}
		if err == nil {
			if task.PlanID == nil || !identity.ValidUUID(*task.PlanID) {
				err = &Error{Code: "internal_error"}
			} else {
				objectKey = strings.TrimPrefix(destination.Prefix+"/orbitbeacon/"+*task.PlanID+"/"+name, "/")
			}
		}
	default:
		err = &Error{Code: "internal_error"}
	}
	if err != nil {
		fail(err, "storage_permission_denied")
		return
	}
	defer func() { credentials = ObjectCredentials{} }()
	temporary := filepath.Join(outputDir, "."+name+".tmp")
	defer os.Remove(temporary)
	manifest := Manifest{Format: "orbitbeacon-backup", Version: 1, ArchiveID: task.ID, CreatedAt: created, ServerVersion: s.opts.ServerVersion, SchemaVersion: storage.CurrentSchemaVersion, HistorySchemaVersion: 1, Categories: task.Categories, Snapshots: payloads.Snapshots}
	info, err := WriteArchive(ctx, temporary, manifest, payloads.Files, password)
	password = ""
	if err != nil {
		fail(err, "internal_error")
		return
	}
	location := filepath.Join(outputDir, name)
	if task.Kind == "backup_remote" {
		location = destination.Endpoint + "/" + destination.Bucket + "/" + objectKey
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE backup_tasks SET phase='storing',artifact_name=?,artifact_bytes=?,artifact_sha256=?,result_location=? WHERE id=? AND state='running'`, name, info.Bytes, info.SHA256, location, task.ID); err != nil {
		fail(err, "internal_error")
		return
	}
	task.Phase = "storing"
	task.ArtifactName = &name
	task.ArtifactSHA256 = &info.SHA256
	task.ResultLocation = &location
	if ctx.Err() != nil {
		return
	}
	if task.Kind == "backup_remote" {
		result, uploadErr := s.opts.Uploader.Upload(ctx, destination, credentials, temporary, objectKey, info.SHA256)
		if uploadErr != nil {
			if result.OutcomeUnknown {
				warning = "storage_outcome_unknown"
			}
			fail(uploadErr, "storage_upload_failed")
			return
		}
	} else {
		// Hard-link installation is atomic and refuses an existing destination.
		// Both names are in the final directory: no full archive copy is required.
		if err = os.Link(temporary, location); err != nil {
			fail(err, "storage_permission_denied")
			return
		}
		if err = os.Remove(temporary); err != nil {
			warning = "storage_outcome_unknown"
			fail(err, "internal_error")
			return
		}
		if err = syncWorkerDirectory(outputDir); err != nil {
			warning = "storage_outcome_unknown"
			fail(err, "internal_error")
			return
		}
		if err = syncWorkerDirectory(filepath.Dir(outputDir)); err != nil {
			warning = "storage_outcome_unknown"
			fail(err, "internal_error")
			return
		}
		if task.Kind == "backup_local" {
			if err = s.PruneLocal(ctx, local.Path, *task.PlanID, local.RetainCount); err != nil {
				warning = "retention_cleanup_failed"
			}
		}
	}
	_ = os.RemoveAll(work)
	// From this point an external publication has definitely succeeded. Never
	// return to packaging or publication, even if the database is unavailable.
	s.finish(ctx, task, "succeeded", "", warning)
}

func (s *Service) finish(ctx context.Context, task Task, state, code, warning string) bool {
	finished := s.opts.Now().UTC()
	for ctx.Err() == nil {
		err := s.finishTransaction(ctx, task, state, code, warning, finished)
		if err == nil {
			s.signal()
			return true
		}
		// Commit errors may mean the commit succeeded. Read both durable facts; a
		// terminal task without its notification is never accepted as completion.
		var current string
		var notifications int
		readErr := s.db.QueryRowContext(ctx, `SELECT state,(SELECT count(*) FROM inbox_notifications WHERE kind='task' AND source_id=backup_tasks.id AND transition=?) FROM backup_tasks WHERE id=?`, state, task.ID).Scan(&current, &notifications)
		if readErr == nil && current == state && notifications == 1 {
			s.signal()
			return true
		}
		if !workerDelay(ctx) {
			return false
		}
	}
	return false
}
func (s *Service) finishTransaction(ctx context.Context, task Task, state, code, warning string, finished time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var expiry any
	if task.Kind == "backup_export" {
		expiry = timekey.Format(finished.Add(24 * time.Hour))
	}
	result, err := tx.ExecContext(ctx, `UPDATE backup_tasks SET state=?,finished_at=?,password_expires_at=?,artifact_expires_at=?,artifact_cleanup_after=?,input_credentials_ciphertext=NULL,error_code=?,warning_code=? WHERE id=? AND state='running'`, state, timekey.Format(finished), timekey.Format(finished.Add(90*24*time.Hour)), expiry, expiry, code, warning, task.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("task is not running")
	}
	projected, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE id=?`, task.ID))
	if err != nil {
		return err
	}
	// Snapshot only safe Task fields; password is explicitly availability-only.
	details, err := json.Marshal(struct {
		Task
		HasPassword bool `json:"has_password"`
	}{Task: projected, HasPassword: passwordEligible(projected)})
	if err != nil {
		return err
	}
	severity := "info"
	if state == "failed" || warning != "" {
		severity = "warning"
	}
	if err = inbox.Insert(ctx, tx, inbox.Event{Kind: "task", SourceID: task.ID, Transition: state, Audience: "admin", Severity: severity, CreatedAt: finished, Details: details}); err != nil {
		return err
	}
	if s.opts.beforeTerminal != nil {
		if err = s.opts.beforeTerminal(tx); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if s.opts.afterTerminalCommit != nil {
		return s.opts.afterTerminalCommit()
	}
	return nil
}
func safeWorkerCode(err error, fallback string) string {
	var coded *Error
	if errors.As(err, &coded) {
		switch coded.Code {
		case "snapshot_failed", "history_unavailable", "archive_too_large", "disk_full", "storage_permission_denied", "storage_unreachable", "storage_auth_failed", "storage_upload_failed", "secret_unreadable", "interrupted", "internal_error":
			return coded.Code
		}
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return "disk_full"
	}
	if errors.Is(err, os.ErrPermission) {
		return "storage_permission_denied"
	}
	var network net.Error
	if errors.As(err, &network) {
		return "storage_unreachable"
	}
	return fallback
}

// cleanupWork only runs before claiming any task. UUID directories with live
// queued/running references are left untouched; no user-selected paths enter it.
func (s *Service) cleanupWork(ctx context.Context) error {
	root := filepath.Join(s.opts.DataDir, "backup-work")
	dir, err := openCleanupDirectory(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	var first error
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		names, readErr := dir.Readdirnames(1)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if len(names) == 0 {
			return first
		}
		id := names[0]
		if identity.ValidUUID(id) {
			var active bool
			if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_tasks WHERE id=? AND state IN ('queued','running') AND restored=0)`, id).Scan(&active); err != nil {
				return err
			}
			if !active {
				work := filepath.Join(root, id)
				if err = securefs.ValidateDirectory(work); err == nil {
					if err = os.RemoveAll(work); err != nil && first == nil {
						first = err
					}
				}
			}
		}
		if readErr == io.EOF {
			return first
		}
	}
}
