package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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

// DependencyConflict 只包含固定的依赖类型和至多一个 UUID。
// CLI 调用方可直接显示该详情，无需解开任意 SQL 错误。
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

// 每个查询和目标标识符仅由固定的注册表或下方显式的临时状态列表提供。
// 绝不执行归档 DDL。
func copyRestoreRows(ctx context.Context, source *sql.DB, destination *sql.Tx, name, columns, query string, transform restoreRowTransform) error {
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return &Error{Code: "restore_invalid_data", Err: err}
	}
	defer rows.Close()
	names := strings.Split(columns, ",")
	if name == "inbox_notifications" {
		columns += ",sequence"
	}
	statement, err := destination.PrepareContext(ctx, "INSERT INTO "+name+" ("+columns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", strings.Count(columns, ",")+1), ",")+")")
	if err != nil {
		return err
	}
	defer statement.Close()
	values := make([]any, len(names))
	pointers := make([]any, len(names))
	for i := range values {
		pointers[i] = &values[i]
	}
	row := values
	if name == "inbox_notifications" {
		row = make([]any, len(values)+1)
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
		if name == "inbox_notifications" {
			sequence, e := nextRestoreSequence(ctx, destination)
			if e != nil {
				return e
			}
			copy(row, values)
			row[len(values)] = sequence
		}
		if _, err = statement.ExecContext(ctx, row...); err != nil {
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

func nextRestoreSequence(ctx context.Context, tx *sql.Tx) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE inbox_sequence SET value=value+1 WHERE id=1 AND value<9223372036854775807`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, errors.New("inbox sequence exhausted")
	}
	var sequence int64
	err = tx.QueryRowContext(ctx, `SELECT value FROM inbox_sequence WHERE id=1`).Scan(&sequence)
	return sequence, err
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
	// 延迟约束让我们能校验完整的最终依赖图，同时强制检查仍然生效，
	// Commit 依然会检查每个外键。
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
			if spec.Name == "users" && selected[Configuration] {
				// 待确认密钥属于临时流程，恢复后必须重新开始。
				transform = func(names []string, values []any) error {
					values[rowIndex(names, "totp_pending_ciphertext")] = nil
					values[rowIndex(names, "totp_pending_at")] = nil
					return nil
				}
			}
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
				query = "SELECT id,last_seen,system,remote_ip,geo_country,geo_location,geo_checked_at FROM nodes"
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
					var system string
					var remoteIP, country, location, checked string
					if e = rows.Scan(&node, &last, &system, &remoteIP, &country, &location, &checked); e != nil {
						break
					}
					if e = check(nil, []any{node}); e != nil {
						break
					}
					_, e = tx.ExecContext(ctx, `UPDATE nodes SET last_seen=?,system=?,remote_ip=?,geo_country=?,geo_location=?,geo_checked_at=? WHERE id=?`, last, system, remoteIP, country, location, checked, node)
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
						if inbox.ValidateDetails("task", details) != nil {
							return invalidPayload("backup_tasks", id)
						}
						notificationID, e := uuid.UUID(rand.Reader)
						if e != nil {
							return e
						}
						var exists bool
						if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_notifications WHERE kind='task' AND source_id=? AND transition='failed')`, id).Scan(&exists); e != nil || exists {
							return e
						}
						sequence, e := nextRestoreSequence(ctx, tx)
						if e != nil {
							return e
						}
						_, e = tx.ExecContext(ctx, `INSERT INTO inbox_notifications(sequence,id,kind,source_id,transition,severity,created_at,details_json) VALUES(?,?,'task',?,'failed','warning',?,?)`, sequence, notificationID, id, timekey.Format(now), string(details))
						return e
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
	auditID, err := uuid.UUID(rand.Reader)
	if err != nil {
		return err
	}
	details, err := json.Marshal(map[string]any{"actor_kind": "restore", "archive_id": archiveID, "state": "succeeded"})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,action,target_id,created_at,details) VALUES(?,NULL,'backup.restore',?,?,?)`, auditID, archiveID, timekey.Format(now), string(details)); err != nil {
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
				// 仅用一个受限 UUID 标识失败行，不暴露值、SQL 错误、
				// 归档字符串或任意表名。
				column := "id"
				if name == "inbox_reads" || name == "recovery_codes" {
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
