package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/timekey"
)

// ErrSchemaTooNew reports a database written by a newer OrbitBeacon release.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

type Migration struct {
	Version int
	SQL     string
	Apply   func(context.Context, *sql.Tx) error
	// DisableForeignKeys reserves one connection and disables enforcement before
	// BEGIN for SQLite's generalized table rebuild procedure, restoring it after.
	DisableForeignKeys bool
}

// CurrentSchemaVersion is the highest schema version produced by this binary.
const CurrentSchemaVersion = 16

// Open opens a persistent local database. The driver applies connection-local
// pragmas to every pooled connection, not just the migration connection.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	// Keep the retired version 12 slot so existing migration ledgers remain valid.
	return OpenWithMigrations(ctx, path, []Migration{{Version: 1, SQL: foundationSchema}, {Version: 2, SQL: sessionsExpirySchema}, {Version: 3, SQL: reportIdentitySchema}, {Version: 4, SQL: enrollmentTombstoneSchema}, {Version: 5, Apply: repairReportSelections}, {Version: 6, SQL: recoverableCSRFSchema}, {Version: 7, SQL: auditOrderingSchema}, {Version: 8, SQL: enrollmentOrderingSchema}, {Version: 9, Apply: normalizeTimestampColumns}, {Version: 10, SQL: alertsSchema}, {Version: 11, SQL: notificationsSchema}, {Version: 12}, {Version: 13, SQL: trafficSchema}, {Version: 14, SQL: probesSchema, Apply: rebuildProbeAlertRules, DisableForeignKeys: true}, {Version: 15, SQL: "DROP TABLE IF EXISTS api_tokens;"}, {Version: 16, SQL: backupsSchema}})
}

// OpenWithMigrations applies an independent schema using the same secure file
// checks and SQLite connection settings as the main database.
func OpenWithMigrations(ctx context.Context, path string, migrations []Migration) (*sql.DB, error) {
	absolute, _, err := validateDatabasePath(path)
	if err != nil {
		return nil, err
	}
	// SQLite derives WAL and shared-memory permissions from the main database.
	// Create it privately before the driver can apply its default creation mode.
	file, err := securefs.OpenFile(absolute, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		file, err = securefs.OpenFile(absolute, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("prepare database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close prepared database: %w", err)
	}
	u := url.URL{Scheme: "file", Path: absolute}
	q := url.Values{"_pragma": {"journal_mode(WAL)", "foreign_keys(ON)", "busy_timeout(5000)", "synchronous(FULL)", "mmap_size(0)", "cache_size(-8192)"}, "_txlock": {"immediate"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(3)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := Migrate(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenReader opens an existing database through a separate read-only pool.
// Open must run first so the database exists and all migrations are complete.
func OpenReader(ctx context.Context, path string) (*sql.DB, error) {
	absolute, exists, err := validateDatabasePath(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("open database reader: %w", os.ErrNotExist)
	}
	u := url.URL{Scheme: "file", Path: absolute}
	u.RawQuery = url.Values{
		"mode":    {"ro"},
		"_pragma": {"query_only(ON)", "busy_timeout(5000)", "mmap_size(0)", "cache_size(-8192)"},
	}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database reader: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect database reader: %w", err)
	}
	return db, nil
}

func validateDatabasePath(path string) (string, bool, error) {
	if err := config.ValidatePath(path); err != nil {
		return "", false, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	if err := validateLocalFilesystem(absolute); err != nil {
		return "", false, err
	}
	exists := false
	// Inspect every existing component before opening or creating the database.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		componentExists, err := securefs.ValidateFile(absolute + suffix)
		if err != nil {
			return "", false, err
		}
		if suffix == "" {
			exists = componentExists
		}
	}
	return absolute, exists, nil
}

// Migrate atomically applies each unapplied version together with its ledger row.
func Migrate(ctx context.Context, db *sql.DB, migrations []Migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	// An older binary must not run against a schema written by a newer one: it
	// would silently ignore columns and invariants it does not know about.
	known := 0
	for _, migration := range migrations {
		known = max(known, migration.Version)
	}
	var applied sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&applied); err != nil {
		return err
	}
	if applied.Valid && applied.Int64 > int64(known) {
		return fmt.Errorf("%w: database schema version %d is newer than supported version %d", ErrSchemaTooNew, applied.Int64, known)
	}
	for _, migration := range migrations {
		if migration.Version < 1 {
			return fmt.Errorf("migration version must be positive")
		}
		if err := applyMigration(ctx, db, migration); err != nil {
			return fmt.Errorf("migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration Migration) (retErr error) {
	begin := db.BeginTx
	if migration.DisableForeignKeys {
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() {
			// A canceled migration must still restore this connection's pragma.
			restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, restoreErr := conn.ExecContext(restoreCtx, "PRAGMA foreign_keys=ON")
			if restoreErr != nil {
				// Never return a connection with enforcement disabled to the pool.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			retErr = errors.Join(retErr, restoreErr, conn.Close())
		}()
		// SQLite ignores this pragma inside a transaction. Reserve the same
		// connection for disabling enforcement, BEGIN, and restoring it later.
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			return err
		}
		begin = conn.BeginTx
	}
	tx, err := begin(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version = ?", migration.Version).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return tx.Commit()
	}
	if migration.SQL != "" {
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			return err
		}
	}
	if migration.Apply != nil {
		if err := migration.Apply(ctx, tx); err != nil {
			return err
		}
	}
	if migration.DisableForeignKeys {
		rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
		if err != nil {
			return err
		}
		violation := rows.Next()
		checkErr := errors.Join(rows.Err(), rows.Close())
		if violation {
			return errors.Join(errors.New("foreign_key_check failed after table rebuild"), checkErr)
		}
		if checkErr != nil {
			return checkErr
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)", migration.Version, timekey.Format(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

const foundationSchema = `
CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('admin','viewer')), created_at TEXT NOT NULL
);
CREATE TABLE sessions (
 token_hash BLOB PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 csrf_hash BLOB NOT NULL, expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE groups (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, desired_config TEXT NOT NULL DEFAULT '{}');
CREATE TABLE nodes (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
 group_id TEXT REFERENCES groups(id) ON DELETE SET NULL,
 region TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', notes TEXT NOT NULL DEFAULT '',
 token_hash BLOB UNIQUE, installation_id TEXT, first_enrolled_at TEXT, last_seen TEXT,
 system TEXT NOT NULL DEFAULT '{}', desired_revision INTEGER NOT NULL DEFAULT 1,
 applied_revision INTEGER NOT NULL DEFAULT 0, public_settings TEXT NOT NULL DEFAULT '{}',
 report_interval_seconds INTEGER NOT NULL DEFAULT 10, disk_interval_seconds INTEGER NOT NULL DEFAULT 60,
 interfaces TEXT NOT NULL DEFAULT '[]', disk_paths TEXT NOT NULL DEFAULT '[]',
 probe_targets TEXT NOT NULL DEFAULT '[]', billing_day INTEGER NOT NULL DEFAULT 1,
 quota_bytes TEXT NOT NULL DEFAULT '0', created_at TEXT NOT NULL
);
CREATE TABLE node_tags (node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, tag TEXT NOT NULL, PRIMARY KEY(node_id,tag));
CREATE TABLE enrollment_tokens (
 id TEXT PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE, node_id TEXT REFERENCES nodes(id) ON DELETE CASCADE,
 group_id TEXT REFERENCES groups(id) ON DELETE SET NULL, expires_at TEXT NOT NULL,
 max_uses INTEGER NOT NULL DEFAULT 1, uses INTEGER NOT NULL DEFAULT 0, revoked_at TEXT
);
CREATE TABLE enrollment_claims (
 enrollment_id TEXT NOT NULL REFERENCES enrollment_tokens(id) ON DELETE CASCADE,
 installation_id TEXT NOT NULL, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 token_hash BLOB NOT NULL, claimed_at TEXT NOT NULL, PRIMARY KEY(enrollment_id,installation_id)
);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE audit_events (
 id TEXT PRIMARY KEY, actor_id TEXT, action TEXT NOT NULL, target_id TEXT,
 created_at TEXT NOT NULL, details TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE update_jobs (id TEXT PRIMARY KEY, release_seq INTEGER NOT NULL, created_at TEXT NOT NULL);
-- Node IDs deliberately survive asset deletion so cancellation remains observable.
CREATE TABLE update_job_nodes (
 job_id TEXT NOT NULL REFERENCES update_jobs(id) ON DELETE CASCADE,
 node_id TEXT NOT NULL, status TEXT NOT NULL, PRIMARY KEY(job_id,node_id)
);
`

const sessionsExpirySchema = `CREATE INDEX sessions_expiry ON sessions(expires_at);`

const auditOrderingSchema = `CREATE INDEX audit_events_created_id ON audit_events(created_at DESC, id DESC);`

const enrollmentOrderingSchema = `
CREATE INDEX enrollment_tokens_expiry_id ON enrollment_tokens(expires_at DESC, id);
CREATE INDEX enrollment_tokens_node ON enrollment_tokens(node_id);
CREATE INDEX enrollment_tokens_group ON enrollment_tokens(group_id);
CREATE INDEX enrollment_claims_node ON enrollment_claims(node_id);
CREATE INDEX nodes_group ON nodes(group_id);
CREATE INDEX update_job_nodes_node ON update_job_nodes(node_id);
`

// Preserve existing migration versions: an installed database may already have
// applied the independent sessions-expiry index migration.
const reportIdentitySchema = `
ALTER TABLE nodes ADD COLUMN run_generation TEXT NOT NULL DEFAULT '0';
ALTER TABLE nodes ADD COLUMN report_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN config_error TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX nodes_installation_identity ON nodes(installation_id) WHERE installation_id IS NOT NULL;
`

// Receipts outlive asset deletion so an exact batch retry cannot recreate it.
const enrollmentTombstoneSchema = `
CREATE TABLE enrollment_claims_retained (
 enrollment_id TEXT NOT NULL REFERENCES enrollment_tokens(id) ON DELETE CASCADE,
 installation_id TEXT NOT NULL, node_id TEXT NOT NULL,
 token_hash BLOB NOT NULL, claimed_at TEXT NOT NULL, PRIMARY KEY(enrollment_id,installation_id)
);
INSERT INTO enrollment_claims_retained SELECT enrollment_id, installation_id, node_id, token_hash, claimed_at FROM enrollment_claims;
DROP TABLE enrollment_claims;
ALTER TABLE enrollment_claims_retained RENAME TO enrollment_claims;
`

type timestampColumn struct {
	table  string
	column string
}

func normalizeTimestampColumns(ctx context.Context, tx *sql.Tx) error {
	for _, field := range []timestampColumn{
		{table: "users", column: "created_at"},
		{table: "nodes", column: "first_enrolled_at"},
		{table: "nodes", column: "last_seen"},
		{table: "nodes", column: "created_at"},
		{table: "enrollment_tokens", column: "expires_at"},
		{table: "enrollment_tokens", column: "revoked_at"},
		{table: "enrollment_claims", column: "claimed_at"},
		{table: "audit_events", column: "created_at"},
		{table: "update_jobs", column: "created_at"},
	} {
		rows, err := tx.QueryContext(ctx, "SELECT rowid, "+field.column+" FROM "+field.table)
		if err != nil {
			return fmt.Errorf("read %s.%s: %w", field.table, field.column, err)
		}
		type value struct {
			rowid int64
			raw   sql.NullString
		}
		values := make([]value, 0)
		for rows.Next() {
			var item value
			if err := rows.Scan(&item.rowid, &item.raw); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan %s.%s: %w", field.table, field.column, err)
			}
			values = append(values, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read %s.%s: %w", field.table, field.column, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close %s.%s: %w", field.table, field.column, err)
		}
		for _, item := range values {
			if !item.raw.Valid {
				continue
			}
			parsed, err := timekey.Parse(item.raw.String)
			if err != nil {
				return fmt.Errorf("parse %s.%s row %d: %w", field.table, field.column, item.rowid, err)
			}
			canonical := timekey.Format(parsed)
			if canonical == item.raw.String {
				continue
			}
			if _, err := tx.ExecContext(ctx, "UPDATE "+field.table+" SET "+field.column+" = ? WHERE rowid = ?", canonical, item.rowid); err != nil {
				return fmt.Errorf("normalize %s.%s row %d: %w", field.table, field.column, item.rowid, err)
			}
		}
	}
	return nil
}

const recoverableCSRFSchema = `ALTER TABLE sessions DROP COLUMN csrf_hash;`
