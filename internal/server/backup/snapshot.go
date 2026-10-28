package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

// SnapshotDatabase uses a dedicated mode=ro connection. Turning query_only off
// permits VACUUM's private output, never writes to or migrates the live source.
func SnapshotDatabase(ctx context.Context, source, output string, version int) error {
	if err := securefs.ValidateDirectory(filepath.Dir(output)); err != nil {
		return err
	}
	reserved, err := securefs.OpenFile(output, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := reserved.Close(); err != nil {
		return err
	}
	reader, err := storage.OpenReader(ctx, source)
	if err != nil {
		return err
	}
	defer reader.Close()
	var got sql.NullInt64
	if err = reader.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&got); err != nil {
		return err
	}
	if !got.Valid || got.Int64 != int64(version) {
		return errors.New("unsupported snapshot schema version")
	}
	conn, err := reader.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "PRAGMA query_only=OFF"); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "VACUUM INTO ?", output); err != nil {
		return err
	}
	f, err := securefs.OpenFile(output, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// SnapshotPayloads contains only selected categories. Full private snapshots
// never become archive entries and must be removed by the work-directory owner.
type SnapshotPayloads struct {
	Files     []ArchiveInput
	Snapshots Snapshots
}

func CreateSnapshotPayloads(ctx context.Context, dataDir, work string, categories []Category, now func() time.Time) (SnapshotPayloads, error) {
	selected, err := ValidateCategories(categories)
	if err != nil {
		return SnapshotPayloads{}, err
	}
	if err = securefs.ValidateDirectory(dataDir); err != nil {
		return SnapshotPayloads{}, err
	}
	if now == nil {
		now = time.Now
	}
	if err = securefs.ValidateParents(work); err != nil {
		return SnapshotPayloads{}, err
	}
	if err = os.MkdirAll(work, 0700); err != nil {
		return SnapshotPayloads{}, err
	}
	if err = securefs.ValidateDirectory(work); err != nil {
		return SnapshotPayloads{}, err
	}
	mainPath := filepath.Join(work, "main-snapshot.sqlite")
	if err = SnapshotDatabase(ctx, filepath.Join(dataDir, "orbitbeacon.db"), mainPath, storage.CurrentSchemaVersion); err != nil {
		return SnapshotPayloads{}, fmt.Errorf("snapshot_failed: %w", err)
	}
	result := SnapshotPayloads{Snapshots: Snapshots{MainAt: now().UTC()}}
	main, err := openPayload(ctx, mainPath)
	if err != nil {
		return SnapshotPayloads{}, err
	}
	defer main.Close()
	var metrics *sql.DB
	for _, category := range selected {
		if category == Monitoring {
			path := filepath.Join(work, "metrics-snapshot.sqlite")
			if err = SnapshotDatabase(ctx, filepath.Join(dataDir, "metrics.db"), path, 1); err != nil {
				return SnapshotPayloads{}, &Error{Code: "history_unavailable"}
			}
			at := now().UTC()
			result.Snapshots.MetricsAt = &at
			metrics, err = openPayload(ctx, path)
			if err != nil {
				return SnapshotPayloads{}, &Error{Code: "history_unavailable"}
			}
			defer metrics.Close()
		}
	}
	var key []byte
	for _, category := range selected {
		if category == Configuration {
			key, err = datadir.LoadKey(filepath.Join(dataDir, "secrets.key"))
			if err != nil {
				return SnapshotPayloads{}, &Error{Code: "secret_unreadable"}
			}
			defer clear(key)
		}
	}
	for _, category := range selected {
		name := string(category) + ".sqlite"
		path := filepath.Join(work, name)
		payload, err := createPayload(ctx, path, category)
		if err != nil {
			return SnapshotPayloads{}, err
		}
		err = projectCategory(ctx, payload, main, metrics, category, now())
		if err == nil {
			err = ValidatePayloadSchema(ctx, payload, category)
		}
		if err == nil {
			err = ValidatePayloadData(ctx, payload, category, key)
		}
		if err == nil {
			err = ValidateSQLite(ctx, payload)
		}
		err = errors.Join(err, payload.Close())
		if err != nil {
			return SnapshotPayloads{}, err
		}
		result.Files = append(result.Files, ArchiveInput{Name: name, Path: path})
		if category == Configuration {
			keyPath := filepath.Join(work, "secrets.key")
			file, err := securefs.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return SnapshotPayloads{}, err
			}
			_, writeErr := file.Write(key)
			if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
				return SnapshotPayloads{}, err
			}
			result.Files = append(result.Files, ArchiveInput{Name: "secrets.key", Path: keyPath})
		}
	}
	return result, nil
}

func projectCategory(ctx context.Context, destination, main, metrics *sql.DB, category Category, asOf time.Time) error {
	// Buckets are from a separately consistent snapshot. The main snapshot's node
	// set defines orphan exclusion; observations are not inferred from live memory.
	for _, spec := range tablesFor(category) {
		source := main
		if spec.Source == "metrics" {
			source = metrics
		}
		if source == nil {
			return &Error{Code: "history_unavailable"}
		}
		query := "SELECT " + columnNames(spec) + " FROM " + spec.Name
		if spec.Name == "node_observations" {
			query = "SELECT id,last_seen,system,applied_revision,config_error FROM nodes"
		}
		if spec.Name == "nodes" {
			query = `SELECT id,name,CASE WHEN status='disabled' THEN 'disabled' WHEN token_hash IS NOT NULL THEN 'unknown' ELSE 'pending' END,group_id,region,provider,notes,created_at,public_settings,desired_revision,report_interval_seconds,disk_interval_seconds,interfaces,disk_paths,probe_targets,billing_day,quota_bytes,token_hash,installation_id,first_enrolled_at,run_generation,report_session_id FROM nodes`
		}
		if spec.Where != "" {
			query += " WHERE " + spec.Where
		}
		if spec.Name == "backup_tasks" {
			query += ` AND (finished_at IS NULL OR finished_at>=?)`
		}
		var queryArgs []any
		if spec.Name == "backup_tasks" {
			queryArgs = append(queryArgs, timekey.Format(asOf.Add(-90*24*time.Hour)))
		}
		rows, err := source.QueryContext(ctx, query, queryArgs...)
		if err != nil {
			return err
		}
		tx, err := destination.BeginTx(ctx, nil)
		if err != nil {
			rows.Close()
			return err
		}
		statement, err := tx.PrepareContext(ctx, "INSERT INTO "+spec.Name+" ("+columnNames(spec)+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(spec.Columns)), ",")+")")
		if err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		values := make([]any, len(spec.Columns))
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		for rows.Next() {
			if err = rows.Scan(pointers...); err != nil {
				break
			}
			if spec.Name == "buckets" {
				var exists bool
				err = main.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM nodes WHERE id=?)", values[0]).Scan(&exists)
				if err != nil {
					break
				}
				if !exists {
					continue
				}
			}
			for i, column := range spec.Columns {
				switch {
				case spec.Name == "notification_deliveries" && column.Name == "lease_until":
					values[i] = ""
				case spec.Name == "backup_tasks" && column.Name == "restored":
					values[i] = int64(1)
				}
			}
			if _, err = statement.ExecContext(ctx, values...); err != nil {
				break
			}
		}
		err = errors.Join(err, rows.Err(), rows.Close(), statement.Close())
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
