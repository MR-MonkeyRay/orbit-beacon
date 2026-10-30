package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

const retainedTaskColumns = "id,kind,state,phase,plan_id,actor_id,categories_json,destination_json,input_credentials_ciphertext,scheduled_at,created_at,started_at,finished_at,artifact_name,artifact_bytes,artifact_sha256,artifact_expires_at,artifact_deleted_at,artifact_cleanup_after,result_location,password_ciphertext,password_expires_at,error_code,warning_code,restored"

type restoreSecrets struct {
	dataDir   string
	finalKey  []byte
	replacing bool
	old, next *secretbox.Box
}

func (s *restoreSecrets) rekey(id string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return ciphertext, nil
	}
	if s.old == nil {
		var err error
		if s.replacing {
			key, e := datadir.LoadKey(filepath.Join(s.dataDir, "secrets.key"))
			if e != nil {
				return nil, &Error{Code: "restore_secret_conflict", Err: e}
			}
			s.old, err = secretbox.New(key, "orbitbeacon backup secrets v1")
			clear(key)
			if err == nil {
				s.next, err = secretbox.New(s.finalKey, "orbitbeacon backup secrets v1")
			}
		} else {
			s.old, err = secretbox.New(s.finalKey, "orbitbeacon backup secrets v1")
		}
		if err != nil {
			return nil, &Error{Code: "restore_secret_conflict", Err: err}
		}
	}
	plain, err := s.old.Open("task-password:"+id, ciphertext)
	if err != nil {
		return nil, &Error{Code: "restore_secret_conflict", Err: err}
	}
	defer clear(plain)
	if len(plain) != 16 {
		return nil, &Error{Code: "restore_secret_conflict"}
	}
	for _, c := range plain {
		if !strings.ContainsRune(passwordAlphabet, rune(c)) {
			return nil, &Error{Code: "restore_secret_conflict"}
		}
	}
	if !s.replacing {
		return ciphertext, nil
	}
	sealed, err := s.next.Seal("task-password:"+id, plain)
	if err != nil {
		return nil, &Error{Code: "restore_secret_conflict", Err: err}
	}
	return sealed, nil
}

// DependencyConflict contains only a fixed dependency type and at most one UUID.
// CLI callers may display this detail without unwrapping arbitrary SQL errors.
type DependencyConflict struct {
	Type string
	ID   string
}

func (e *DependencyConflict) Error() string {
	if e.ID == "" {
		return e.Type
	}
	return e.Type + " " + e.ID
}

func dependencyConflict(kind string, id any) error {
	safe, _ := id.(string)
	if !uuid.ValidUUID(safe) {
		safe = ""
	}
	return &Error{Code: "restore_dependency_conflict", Err: &DependencyConflict{Type: kind, ID: safe}}
}

type restoreRowTransform func([]string, []any) error

// Each query and destination identifier is supplied only by the fixed registry
// or the explicit ephemeral-state lists below. Archive DDL is never executed.
func copyRestoreRows(ctx context.Context, source *sql.DB, destination *sql.Tx, name, columns, query string, transform restoreRowTransform) error {
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return &Error{Code: "restore_invalid_data", Err: err}
	}
	defer rows.Close()
	names := strings.Split(columns, ",")
	statement, err := destination.PrepareContext(ctx, "INSERT INTO "+name+" ("+columns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")+")")
	if err != nil {
		return err
	}
	defer statement.Close()
	values := make([]any, len(names))
	pointers := make([]any, len(names))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err = rows.Scan(pointers...); err != nil {
			return &Error{Code: "restore_invalid_data", Err: err}
		}
		if transform != nil {
			if err = transform(names, values); err != nil {
				return err
			}
		}
		if _, err = statement.ExecContext(ctx, values...); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var id any
			if len(values) > 0 {
				id = values[0]
			}
			return dependencyConflict(name, id)
		}
	}
	return rows.Err()
}
func rowIndex(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	panic("unknown fixed restore column")
}

func buildRestoreState(ctx context.Context, main, metrics *sql.DB, sources map[Category]*sql.DB, targetMetrics *sql.DB, selected map[Category]bool, key []byte, dataDir, archiveID string, now time.Time) error {
	tx, err := main.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Deferral allows us to validate the entire final dependency graph, while
	// enforcement remains enabled and Commit still checks every foreign key.
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return err
	}
	mt, err := metrics.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer mt.Rollback()
	if source := sources[Configuration]; source != nil {
		for _, spec := range tablesFor(Configuration) {
			columns := columnNames(spec)
			query := "SELECT " + columns + " FROM " + spec.Name
			if spec.Name == "backup_plans" && !selected[Configuration] {
				columns += ",next_run_at"
				query = "SELECT " + columns + " FROM backup_plans"
			}
			if spec.Where != "" {
				query += " WHERE " + spec.Where
			}
			var transform restoreRowTransform
			if spec.Name == "nodes" {
				transform = func(names []string, values []any) error {
					status := rowIndex(names, "status")
					if values[status] != "disabled" {
						values[status] = "pending"
						if values[rowIndex(names, "token_hash")] != nil {
							values[status] = "unknown"
						}
					}
					return nil
				}
			}
			if spec.Name == "backup_plans" && selected[Configuration] {
				transform = func(names []string, values []any) error { values[rowIndex(names, "enabled")] = int64(0); return nil }
			}
			if err = copyRestoreRows(ctx, source, tx, spec.Name, columns, query, transform); err != nil {
				return err
			}
		}
		if !selected[Configuration] {
			for _, spec := range []struct{ name, columns string }{
				{"sessions", "token_hash,user_id,expires_at"},
				{"enrollment_tokens", "id,token_hash,node_id,group_id,expires_at,max_uses,uses,revoked_at"},
				{"enrollment_claims", "enrollment_id,installation_id,node_id,token_hash,claimed_at"},
			} {
				if err = copyRestoreRows(ctx, source, tx, spec.name, spec.columns, "SELECT "+spec.columns+" FROM "+spec.name, nil); err != nil {
					return err
				}
			}
		}
	}
	if source := sources[Monitoring]; source != nil {
		for _, spec := range tablesFor(Monitoring) {
			reader := source
			destination := tx
			query := "SELECT " + columnNames(spec) + " FROM " + spec.Name
			if spec.Source == "metrics" {
				destination = mt
				if !selected[Monitoring] {
					reader = targetMetrics
				}
			}
			if spec.Name == "node_observations" && !selected[Monitoring] {
				query = "SELECT id,last_seen,system,agent_runtime FROM nodes"
			}
			if reader == nil {
				return &Error{Code: "restore_invalid_data"}
			}
			check := func(names []string, values []any) error {
				var exists bool
				if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM nodes WHERE id=?)", values[0]).Scan(&exists); e != nil {
					return e
				}
				if !exists {
					return dependencyConflict(spec.Name, values[0])
				}
				return nil
			}
			if spec.Name == "node_observations" {
				rows, e := reader.QueryContext(ctx, query)
				if e != nil {
					return &Error{Code: "restore_invalid_data", Err: e}
				}
				for rows.Next() {
					var node string
					var last any
					var system, runtime string
					if e = rows.Scan(&node, &last, &system, &runtime); e != nil {
						break
					}
					if e = check(nil, []any{node}); e != nil {
						break
					}
					if runtime != "" {
						var settings v1.RuntimeSettings
						if objectJSON(runtime, &settings) != nil || settings.Validate() != nil {
							e = invalidPayload("node_observations", node)
							break
						}
						if settings.Interfaces == nil {
							settings.Interfaces = []string{}
						}
						if settings.DiskPaths == nil {
							settings.DiskPaths = []string{}
						}
						if settings.ProbeTargets == nil {
							settings.ProbeTargets = []string{}
						}
						encoded, err := json.Marshal(settings)
						if err != nil {
							e = invalidPayload("node_observations", node)
							break
						}
						runtime = string(encoded)
					}
					_, e = tx.ExecContext(ctx, `UPDATE nodes SET last_seen=?,system=?,agent_runtime=? WHERE id=?`, last, system, runtime, node)
					if e != nil {
						break
					}
				}
				e = errors.Join(e, rows.Err(), rows.Close())
				if e != nil {
					return e
				}
				continue
			}
			if err = copyRestoreRows(ctx, reader, destination, spec.Name, columnNames(spec), query, check); err != nil {
				return err
			}
		}
	}
	if source := sources[Events]; source != nil {
		secrets := restoreSecrets{dataDir: dataDir, finalKey: key, replacing: selected[Configuration]}
		for _, spec := range tablesFor(Events) {
			columns := columnNames(spec)
			query := "SELECT " + columns + " FROM " + spec.Name
			var transform restoreRowTransform
			if spec.Name == "inbox_notifications" {
				query += " ORDER BY created_at,id"
			}
			if spec.Name == "backup_tasks" {
				if selected[Events] {
					transform = func(names []string, values []any) error { values[rowIndex(names, "restored")] = int64(1); return nil }
				} else {
					columns = retainedTaskColumns
					query = "SELECT " + columns + " FROM backup_tasks"
					transform = func(names []string, values []any) error {
						index := func(name string) int { return rowIndex(names, name) }
						id, _ := values[index("id")].(string)
						cipher, _ := values[index("password_ciphertext")].([]byte)
						if len(cipher) > 0 {
							sealed, e := secrets.rekey(id, cipher)
							if e != nil {
								return e
							}
							values[index("password_ciphertext")] = sealed
						}
						state, _ := values[index("state")].(string)
						values[index("input_credentials_ciphertext")] = nil
						if state != "queued" && state != "running" {
							return nil
						}
						values[index("state")] = "failed"
						values[index("error_code")] = "restore_interrupted"
						if values[index("phase")] == "storing" {
							values[index("warning_code")] = "storage_outcome_unknown"
						}
						values[index("finished_at")] = timekey.Format(now)
						values[index("password_expires_at")] = timekey.Format(now.Add(90 * 24 * time.Hour))
						if values[index("kind")] == "backup_export" {
							expiry := timekey.Format(now.Add(24 * time.Hour))
							values[index("artifact_expires_at")] = expiry
							values[index("artifact_cleanup_after")] = expiry
						}
						details, e := json.Marshal(map[string]any{"kind": values[index("kind")], "categories": json.RawMessage(fmt.Sprint(values[index("categories_json")])), "state": "failed", "phase": values[index("phase")], "error_code": "restore_interrupted", "warning_code": values[index("warning_code")], "finished_at": timekey.Format(now)})
						if e != nil {
							return invalidPayload("backup_tasks", id)
						}
						return inbox.Insert(ctx, tx, inbox.Event{Kind: "task", SourceID: id, Transition: "failed", Severity: "warning", CreatedAt: now, Details: details})
					}
				}
			}
			if spec.Name == "notification_deliveries" && !selected[Events] {
				transform = func(names []string, values []any) error {
					state := rowIndex(names, "state")
					values[rowIndex(names, "lease_until")] = ""
					if values[state] == "pending" {
						values[state] = "failed"
						values[rowIndex(names, "last_error_code")] = "restore_interrupted"
						values[rowIndex(names, "updated_at")] = timekey.Format(now)
					}
					return nil
				}
			}
			if err = copyRestoreRows(ctx, source, tx, spec.Name, columns, query, transform); err != nil {
				return err
			}
		}
	}
	if err = validateRestoreDependencies(ctx, tx); err != nil {
		return err
	}
	if err = audit.InsertDetails(audit.WithSystemActor(ctx, "restore"), tx, "backup.restore", archiveID, now, audit.Details{"archive_id": archiveID, "state": "succeeded"}); err != nil {
		return err
	}
	if err = mt.Commit(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return dependencyConflict("foreign_key", nil)
	}
	return nil
}

func validateRestoreDependencies(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return &Error{Code: "restore_dependency_conflict"}
	}
	if !rows.Next() {
		return errors.Join(rows.Err(), rows.Close())
	}
	var name, parent string
	var rowid any
	var fk int
	if err = rows.Scan(&name, &rowid, &parent, &fk); err != nil {
		rows.Close()
		return &Error{Code: "restore_dependency_conflict"}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, category := range []Category{Configuration, Monitoring, Events} {
		for _, spec := range tablesFor(category) {
			if name == spec.Name {
				// A single bounded UUID identifies the failed row without exposing values,
				// SQL errors, archive strings, or arbitrary table names.
				column := "id"
				if name == "inbox_reads" {
					column = "user_id"
				} else if name == "node_tags" {
					column = "node_id"
				} else if name == "alert_rule_channels" {
					column = "rule_id"
				}
				var id string
				_ = tx.QueryRowContext(ctx, "SELECT "+column+" FROM "+spec.Name+" WHERE rowid=?", rowid).Scan(&id)
				return dependencyConflict(spec.Name, id)
			}
		}
	}
	return dependencyConflict("ephemeral_state", nil)
}
