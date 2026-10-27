package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
)

var sqliteSidecars = []string{"-wal", "-shm", "-journal"}
var requiredOrbitBeaconTables = [...]string{
	"users",
	"sessions",
	"groups",
	"nodes",
	"node_tags",
	"enrollment_tokens",
	"enrollment_claims",
	"settings",
	"audit_events",
	"update_jobs",
	"update_job_nodes",
	"schema_migrations",
}

var requiredOrbitBeaconColumns = map[string][]string{
	"users":             {"id", "username", "password_hash", "role", "created_at"},
	"sessions":          {"token_hash", "user_id", "expires_at"},
	"groups":            {"id", "name", "desired_config"},
	"nodes":             {"id", "name", "status", "group_id", "region", "provider", "notes", "token_hash", "installation_id", "first_enrolled_at", "last_seen", "system", "desired_revision", "applied_revision", "public_settings", "report_interval_seconds", "disk_interval_seconds", "interfaces", "disk_paths", "probe_targets", "billing_day", "quota_bytes", "created_at", "run_generation", "report_session_id", "config_error"},
	"node_tags":         {"node_id", "tag"},
	"enrollment_tokens": {"id", "token_hash", "node_id", "group_id", "expires_at", "max_uses", "uses", "revoked_at"},
	"enrollment_claims": {"enrollment_id", "installation_id", "node_id", "token_hash", "claimed_at"},
	"settings":          {"key", "value"},
	"audit_events":      {"id", "actor_id", "action", "target_id", "created_at", "details"},
	"update_jobs":       {"id", "release_seq", "created_at"},
	"update_job_nodes":  {"job_id", "node_id", "status"},
	"schema_migrations": {"version", "applied_at"},
}

type orbitBeaconIndexContract struct {
	name       string
	table      string
	columns    []string
	unique     bool
	partial    bool
	definition string
}

var requiredOrbitBeaconIndexes = []orbitBeaconIndexContract{
	{name: "sessions_user", table: "sessions", columns: []string{"user_id"}},
	{name: "sessions_expiry", table: "sessions", columns: []string{"expires_at"}},
	{name: "nodes_installation_identity", table: "nodes", columns: []string{"installation_id"}, unique: true, partial: true, definition: "CREATE UNIQUE INDEX nodes_installation_identity ON nodes(installation_id) WHERE installation_id IS NOT NULL"},
	{name: "audit_events_created_id", table: "audit_events", columns: []string{"created_at", "id"}},
	{name: "enrollment_tokens_expiry_id", table: "enrollment_tokens", columns: []string{"expires_at", "id"}},
	{name: "enrollment_tokens_node", table: "enrollment_tokens", columns: []string{"node_id"}},
	{name: "enrollment_tokens_group", table: "enrollment_tokens", columns: []string{"group_id"}},
	{name: "enrollment_claims_node", table: "enrollment_claims", columns: []string{"node_id"}},
	{name: "nodes_group", table: "nodes", columns: []string{"group_id"}},
	{name: "update_job_nodes_node", table: "update_job_nodes", columns: []string{"node_id"}},
}

func backupDatabase(dataDir, output string) (retErr error) {
	if err := config.ValidatePath(output); err != nil {
		return fmt.Errorf("output: %w", err)
	}
	output, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	outputDir := filepath.Dir(output)
	if err := securefs.ValidateParents(output); err != nil {
		return err
	}
	if _, err := os.Lstat(output); err == nil {
		return errors.New("backup output already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	sourcePath := filepath.Join(dataDir, "orbitbeacon.db")
	validated, err := securefs.ValidateFile(sourcePath)
	if err != nil {
		return fmt.Errorf("source database: %w", err)
	}
	if !validated {
		return errors.New("source database does not exist")
	}
	temp, err := os.CreateTemp(outputDir, ".orbitbeacon-backup-*.db")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	tempInfo, err := temp.Stat()
	if err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	committed := false
	outputCreated := false
	defer func() {
		cleanupErr := removeIfExists(tempPath)
		if !committed && outputCreated {
			if info, statErr := os.Stat(output); statErr == nil {
				if os.SameFile(tempInfo, info) {
					if removeErr := os.Remove(output); removeErr != nil {
						cleanupErr = errors.Join(cleanupErr, removeErr)
					}
				}
			} else if !os.IsNotExist(statErr) {
				cleanupErr = errors.Join(cleanupErr, statErr)
			}
		}
		if !committed {
			cleanupErr = errors.Join(cleanupErr, syncDirectory(outputDir))
		}
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup backup: %w", cleanupErr))
		}
	}()
	lock, err := lockDataDir(dataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := prepareDataDir(dataDir); err != nil {
		return err
	}
	db, err := storage.Open(context.Background(), sourcePath)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(context.Background(), "VACUUM INTO ?", tempPath); err != nil {
		_ = db.Close()
		return fmt.Errorf("create consistent backup: %w", err)
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0600); err != nil {
		return err
	}
	validated, err = securefs.ValidateFile(tempPath)
	if err != nil {
		return err
	}
	if !validated {
		return errors.New("backup file disappeared before installation")
	}
	if err := syncFile(tempPath); err != nil {
		return err
	}
	if err := os.Link(tempPath, output); err != nil {
		return fmt.Errorf("install backup: %w", err)
	}
	outputCreated = true
	if err := os.Remove(tempPath); err != nil {
		return err
	}
	if err := syncDirectory(outputDir); err != nil {
		return err
	}
	validated, err = securefs.ValidateFile(output)
	if err != nil {
		return err
	}
	if !validated {
		return errors.New("backup file disappeared after installation")
	}
	committed = true
	return nil
}

func restoreDatabase(dataDir, input string) (retErr error) {
	if err := config.ValidatePath(input); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	validated, err := securefs.ValidateFile(input)
	if err != nil {
		return fmt.Errorf("input: %w", err)
	}
	if !validated {
		return errors.New("input backup file is missing")
	}
	lock, err := lockDataDir(dataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := prepareDataDir(dataDir); err != nil {
		return err
	}
	stage, err := os.CreateTemp(dataDir, ".orbitbeacon-restore-*.db")
	if err != nil {
		return err
	}
	stagePath := stage.Name()
	stageOpen := true
	defer func() {
		if stageOpen {
			if closeErr := stage.Close(); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close restore staging: %w", closeErr))
			}
			stageOpen = false
		}
		if cleanupErr := removeSQLiteArtifacts(stagePath); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup restore staging: %w", cleanupErr))
		}
		if retErr != nil {
			if syncErr := syncDirectory(dataDir); syncErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("sync restore cleanup: %w", syncErr))
			}
		}
	}()
	inputFile, err := securefs.OpenFile(input, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	if _, err := io.Copy(stage, inputFile); err != nil {
		_ = inputFile.Close()
		return fmt.Errorf("copy restore input: %w", err)
	}
	if err := inputFile.Close(); err != nil {
		return err
	}
	if err := stage.Sync(); err != nil {
		return err
	}
	if err := stage.Close(); err != nil {
		stageOpen = false
		return err
	}
	stageOpen = false
	if err := validateSQLiteInput(stagePath); err != nil {
		return err
	}
	if err := os.Chmod(stagePath, 0600); err != nil {
		return err
	}
	validated, err = securefs.ValidateFile(stagePath)
	if err != nil {
		return fmt.Errorf("staged restore: %w", err)
	}
	if !validated {
		return errors.New("staged restore file is missing")
	}
	if err := validateAndCheckpointDatabase(stagePath); err != nil {
		return fmt.Errorf("validate restore database: %w", err)
	}
	live := filepath.Join(dataDir, "orbitbeacon.db")
	liveInfo, err := os.Lstat(live)
	if err == nil {
		validated, err = securefs.ValidateFile(live)
		if err != nil {
			return fmt.Errorf("live database: %w", err)
		}
		if !validated {
			return errors.New("live database file is missing")
		}
		if err := checkpointDatabase(live); err != nil {
			return fmt.Errorf("checkpoint live database before restore: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if liveInfo == nil {
		if err := removeSQLiteSidecars(live); err != nil {
			return err
		}
		if err := syncDirectory(dataDir); err != nil {
			return err
		}
	}
	previous := ""
	if liveInfo != nil {
		previous, err = reservePreviousPath(dataDir)
		if err != nil {
			return err
		}
		if err := os.Link(live, previous); err != nil {
			_ = os.Remove(previous)
			return fmt.Errorf("retain previous database: %w", err)
		}
		if err := syncDirectory(dataDir); err != nil {
			_ = os.Remove(previous)
			_ = syncDirectory(dataDir)
			return err
		}
	}
	if err := os.Rename(stagePath, live); err != nil {
		if previous != "" {
			if removeErr := os.Remove(previous); removeErr != nil && !os.IsNotExist(removeErr) {
				return errors.Join(err, fmt.Errorf("remove partial previous database: %w", removeErr))
			}
			if syncErr := syncDirectory(dataDir); syncErr != nil {
				return errors.Join(err, fmt.Errorf("sync failed restore: %w", syncErr))
			}
		}
		return err
	}
	if err := syncDirectory(dataDir); err != nil {
		return err
	}
	return nil
}

func validateSQLiteInput(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("input backup is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	header := make([]byte, len("SQLite format 3\x00"))
	_, readErr := io.ReadFull(file, header)
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("read SQLite header: %w", errors.Join(readErr, closeErr))
	}
	if closeErr != nil {
		return closeErr
	}
	if !bytes.Equal(header, []byte("SQLite format 3\x00")) {
		return errors.New("input backup is not a SQLite database")
	}
	return nil
}

func validateAndCheckpointDatabase(path string) error {
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		return err
	}
	if err := validateDatabase(context.Background(), db); err != nil {
		return errors.Join(err, db.Close())
	}
	if err := db.Close(); err != nil {
		return err
	}
	return checkpointDatabase(path)
}

func checkpointDatabase(path string) error {
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		return err
	}
	var busy, logFrames, checkpointedFrames int
	queryErr := db.QueryRowContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames)
	closeErr := db.Close()
	if queryErr != nil {
		return errors.Join(queryErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if busy != 0 || logFrames != checkpointedFrames {
		return fmt.Errorf("checkpoint incomplete: busy=%d log_frames=%d checkpointed_frames=%d", busy, logFrames, checkpointedFrames)
	}
	if err := syncFile(path); err != nil {
		return err
	}
	if err := removeSQLiteSidecars(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func validateDatabase(ctx context.Context, db *sql.DB) error {
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity check failed: %s", integrity)
	}
	for _, table := range requiredOrbitBeaconTables {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil {
			return fmt.Errorf("schema table %q: %w", table, err)
		}
		if count != 1 {
			return fmt.Errorf("schema table %q is missing", table)
		}
		actual, err := tableColumns(ctx, db, table)
		if err != nil {
			return fmt.Errorf("schema columns %q: %w", table, err)
		}
		for _, expected := range requiredOrbitBeaconColumns[table] {
			if !containsString(actual, expected) {
				return fmt.Errorf("schema table %q column %q is missing", table, expected)
			}
		}
	}
	for _, contract := range requiredOrbitBeaconIndexes {
		unique, partial, found, err := indexProperties(ctx, db, contract.table, contract.name)
		if err != nil {
			return fmt.Errorf("schema index %q: %w", contract.name, err)
		}
		if !found {
			return fmt.Errorf("schema index %q on table %q is missing", contract.name, contract.table)
		}
		if unique != contract.unique || partial != contract.partial {
			return fmt.Errorf("schema index %q properties unique=%t partial=%t, want unique=%t partial=%t", contract.name, unique, partial, contract.unique, contract.partial)
		}
		actual, err := indexColumns(ctx, db, contract.name)
		if err != nil {
			return fmt.Errorf("schema index %q columns: %w", contract.name, err)
		}
		if !equalStrings(actual, contract.columns) {
			return fmt.Errorf("schema index %q has columns %v, want %v", contract.name, actual, contract.columns)
		}
		if contract.definition != "" {
			definition, err := indexDefinition(ctx, db, contract.table, contract.name)
			if err != nil {
				return fmt.Errorf("schema index %q definition: %w", contract.name, err)
			}
			if normalizeSQL(definition) != normalizeSQL(contract.definition) {
				return fmt.Errorf("schema index %q definition is %q, want %q", contract.name, definition, contract.definition)
			}
		}
	}
	for version := 1; version <= 9; version++ {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version = ?", version).Scan(&count); err != nil {
			return fmt.Errorf("schema migration %d: %w", version, err)
		}
		if count != 1 {
			return fmt.Errorf("schema migration %d is missing", version)
		}
	}
	var unsupported int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version NOT BETWEEN 1 AND 9").Scan(&unsupported); err != nil {
		return fmt.Errorf("schema migration range: %w", err)
	}
	if unsupported != 0 {
		return fmt.Errorf("schema contains %d unsupported migration versions", unsupported)
	}
	return nil
}

func indexDefinition(ctx context.Context, db *sql.DB, table, index string) (string, error) {
	var definition sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND name = ?", table, index).Scan(&definition); err != nil {
		return "", err
	}
	if !definition.Valid {
		return "", errors.New("index has no SQL definition")
	}
	return definition.String, nil
}

func normalizeSQL(definition string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(definition), " "))
	normalized = strings.ReplaceAll(normalized, " (", "(")
	normalized = strings.ReplaceAll(normalized, "( ", "(")
	normalized = strings.ReplaceAll(normalized, " )", ")")
	return strings.TrimSuffix(normalized, ";")
}

func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, rows.Close()
}

func indexProperties(ctx context.Context, db *sql.DB, table, index string) (unique, partial, found bool, err error) {
	rows, err := db.QueryContext(ctx, "PRAGMA index_list("+table+")")
	if err != nil {
		return false, false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var sequence, isUnique, isPartial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &isUnique, &origin, &isPartial); err != nil {
			return false, false, false, err
		}
		if name == index {
			return isUnique != 0, isPartial != 0, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, false, err
	}
	return false, false, false, rows.Close()
}

func indexColumns(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA index_info("+index+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var sequence, columnID int
		var name sql.NullString
		if err := rows.Scan(&sequence, &columnID, &name); err != nil {
			return nil, err
		}
		if !name.Valid {
			return nil, errors.New("index contains an expression column")
		}
		columns = append(columns, name.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, rows.Close()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sidecarPaths(base string) []string {
	paths := make([]string, 0, len(sqliteSidecars))
	for _, suffix := range sqliteSidecars {
		paths = append(paths, base+suffix)
	}
	return paths
}

func removeSQLiteSidecars(base string) error {
	for _, path := range sidecarPaths(base) {
		if err := removeIfExists(path); err != nil {
			return err
		}
	}
	return nil
}
func removeSQLiteArtifacts(base string) error {
	if err := removeIfExists(base); err != nil {
		return err
	}
	return removeSQLiteSidecars(base)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func reservePreviousPath(dataDir string) (string, error) {
	file, err := os.CreateTemp(dataDir, ".orbitbeacon.db.before-restore-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
