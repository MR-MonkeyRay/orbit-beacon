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
	"unicode"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
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
	"traffic_hourly",
	"traffic_daily",
	"probe_latest",
	"probe_hourly",
	"alert_rules",
	"alert_rule_channels",
	"alert_events",
	"notification_channels",
	"notification_rate_limits",
	"notification_deliveries",
}

var requiredOrbitBeaconColumns = map[string][]string{
	"users":                    {"id", "username", "password_hash", "role", "created_at"},
	"sessions":                 {"token_hash", "user_id", "expires_at"},
	"groups":                   {"id", "name", "desired_config"},
	"nodes":                    {"id", "name", "status", "group_id", "region", "provider", "notes", "token_hash", "installation_id", "first_enrolled_at", "last_seen", "system", "desired_revision", "applied_revision", "public_settings", "report_interval_seconds", "disk_interval_seconds", "interfaces", "disk_paths", "probe_targets", "billing_day", "quota_bytes", "created_at", "run_generation", "report_session_id", "config_error"},
	"node_tags":                {"node_id", "tag"},
	"enrollment_tokens":        {"id", "token_hash", "node_id", "group_id", "expires_at", "max_uses", "uses", "revoked_at"},
	"enrollment_claims":        {"enrollment_id", "installation_id", "node_id", "token_hash", "claimed_at"},
	"settings":                 {"key", "value"},
	"audit_events":             {"id", "actor_id", "action", "target_id", "created_at", "details"},
	"update_jobs":              {"id", "release_seq", "created_at"},
	"update_job_nodes":         {"job_id", "node_id", "status"},
	"schema_migrations":        {"version", "applied_at"},
	"traffic_hourly":           {"node_id", "epoch", "period_start", "rx_bytes", "tx_bytes", "updated_at"},
	"traffic_daily":            {"node_id", "day", "rx_bytes", "tx_bytes"},
	"probe_latest":             {"node_id", "target", "ok", "latency_ms", "error", "status_code", "checked_at"},
	"probe_hourly":             {"node_id", "target", "hour", "checks", "failures", "latency_sum_ms"},
	"alert_rules":              {"id", "name", "enabled", "scope_kind", "scope_id", "scope_group_id", "scope_node_id", "metric", "comparator", "threshold", "for_seconds", "severity", "created_at", "updated_at"},
	"alert_rule_channels":      {"rule_id", "channel_id"},
	"alert_events":             {"id", "rule_id", "node_id", "state", "value", "started_at", "resolved_at", "last_notified_at", "rule_name", "node_name", "rule_snapshot"},
	"notification_channels":    {"id", "name", "kind", "enabled", "config_json", "secret_ciphertext", "url_ciphertext", "created_at", "updated_at"},
	"notification_rate_limits": {"channel_id", "window_start", "count"},
	"notification_deliveries":  {"id", "channel_id", "event_id", "state", "attempts", "next_attempt_at", "last_error_code", "lease_until", "payload_json", "coalesced_count", "created_at", "updated_at"},
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
	{name: "traffic_hourly_retention", table: "traffic_hourly", columns: []string{"period_start"}},
	{name: "traffic_daily_retention", table: "traffic_daily", columns: []string{"day"}},
	{name: "probe_hourly_retention", table: "probe_hourly", columns: []string{"hour"}},
	{name: "alert_rules_group", table: "alert_rules", columns: []string{"scope_group_id"}},
	{name: "alert_rules_node", table: "alert_rules", columns: []string{"scope_node_id"}},
	{name: "alert_rule_channels_channel", table: "alert_rule_channels", columns: []string{"channel_id"}},
	{name: "alert_events_active", table: "alert_events", columns: []string{"rule_id", "node_id"}, unique: true, partial: true, definition: "CREATE UNIQUE INDEX alert_events_active ON alert_events(rule_id,node_id) WHERE state='firing'"},
	{name: "alert_events_history", table: "alert_events", columns: []string{"started_at", "id"}},
	{name: "alert_events_state_history", table: "alert_events", columns: []string{"state", "started_at", "id"}},
	{name: "alert_events_retention", table: "alert_events", columns: []string{"resolved_at", "id"}, partial: true, definition: "CREATE INDEX alert_events_retention ON alert_events(resolved_at,id) WHERE state='resolved'"},
	{name: "notification_deliveries_due", table: "notification_deliveries", columns: []string{"next_attempt_at", "id"}, partial: true, definition: "CREATE INDEX notification_deliveries_due ON notification_deliveries(next_attempt_at,id) WHERE state='pending'"},
	{name: "notification_deliveries_channel", table: "notification_deliveries", columns: []string{"channel_id", "state", "next_attempt_at"}},
	{name: "notification_deliveries_retention", table: "notification_deliveries", columns: []string{"updated_at", "id"}, partial: true, definition: "CREATE INDEX notification_deliveries_retention ON notification_deliveries(updated_at,id) WHERE state IN ('sent','failed')"},
	{name: "notification_deliveries_history", table: "notification_deliveries", columns: []string{"created_at", "id"}},
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

func backupDatabase(dataDir, output string) error {
	return backupDatabaseWithMode(dataDir, output, false)
}

func backupDatabaseOnline(dataDir, output string) error {
	return backupDatabaseWithMode(dataDir, output, true)
}

func backupDatabaseWithMode(dataDir, output string, online bool) (retErr error) {
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
	if online {
		if err := securefs.ValidateDirectory(dataDir); err != nil {
			return fmt.Errorf("data directory: %w", err)
		}
		if err := validateLiveSidecars(sourcePath); err != nil {
			return fmt.Errorf("source database sidecars: %w", err)
		}
	}
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
		if online {
			cleanupErr = errors.Join(cleanupErr, removeSQLiteSidecars(tempPath))
		}
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
	var db *sql.DB
	if online {
		db, err = openOnlineBackupSource(sourcePath)
	} else {
		lock, lockErr := datadir.Lock(dataDir)
		if lockErr != nil {
			return lockErr
		}
		defer lock.Close()
		if err := datadir.Prepare(dataDir); err != nil {
			return err
		}
		db, err = storage.Open(context.Background(), sourcePath)
	}
	if err != nil {
		return err
	}
	if err := vacuumBackup(db, tempPath, online); err != nil {
		return errors.Join(err, db.Close())
	}
	if err := db.Close(); err != nil {
		return err
	}
	if online {
		if err := validateSQLiteInput(tempPath); err != nil {
			return err
		}
		// Migrations and checkpointing are confined to the private snapshot.
		if err := validateAndCheckpointDatabase(tempPath); err != nil {
			return fmt.Errorf("validate backup database: %w", err)
		}
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

func openOnlineBackupSource(path string) (*sql.DB, error) {
	ctx := context.Background()
	db, err := storage.OpenReader(ctx, path)
	if err != nil {
		return nil, err
	}
	var version sql.NullInt64
	err = db.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version)
	if err != nil {
		err = fmt.Errorf("source schema version: %w", err)
	} else if !version.Valid || version.Int64 < 1 {
		err = errors.New("source schema migration ledger is empty or invalid")
	} else if version.Int64 > storage.CurrentSchemaVersion {
		err = fmt.Errorf("%w: database schema version %d is newer than supported version %d", storage.ErrSchemaTooNew, version.Int64, storage.CurrentSchemaVersion)
	}
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func vacuumBackup(db *sql.DB, output string, online bool) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if online {
		// query_only also prohibits writes to VACUUM's attached output. The
		// source remains protected by mode=ro for this connection's lifetime.
		if _, err := conn.ExecContext(ctx, "PRAGMA query_only=OFF"); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", output); err != nil {
		return fmt.Errorf("create consistent backup: %w", err)
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
	for version := 1; version <= storage.CurrentSchemaVersion; version++ {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version = ?", version).Scan(&count); err != nil {
			return fmt.Errorf("schema migration %d: %w", version, err)
		}
		if count != 1 {
			return fmt.Errorf("schema migration %d is missing", version)
		}
	}
	var unsupported int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version NOT BETWEEN 1 AND ?", storage.CurrentSchemaVersion).Scan(&unsupported); err != nil {
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

// normalizeSQL ignores case and spacing outside string literals. Literals stay
// verbatim: a predicate such as state='FIRING' differs from state='firing'.
func normalizeSQL(definition string) string {
	var normalized strings.Builder
	quoted, space := false, false
	for _, r := range strings.TrimSpace(definition) {
		switch {
		case r == '\'':
			quoted = !quoted
		case quoted:
		case unicode.IsSpace(r):
			space = true
			continue
		}
		if space {
			normalized.WriteByte(' ')
			space = false
		}
		if !quoted && r != '\'' {
			r = unicode.ToLower(r)
		}
		normalized.WriteRune(r)
	}
	result := normalized.String()
	result = strings.ReplaceAll(result, " (", "(")
	result = strings.ReplaceAll(result, "( ", "(")
	result = strings.ReplaceAll(result, " )", ")")
	return strings.TrimSuffix(result, ";")
}

func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_xinfo("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey, hidden int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey, &hidden); err != nil {
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

func validateLiveSidecars(live string) error {
	for _, suffix := range sqliteSidecars {
		if _, err := securefs.ValidateFile(live + suffix); err != nil {
			return err
		}
	}
	return nil
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
