package storage

import (
	"context"
	"database/sql"
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

// ErrSchemaIncompatible requires a new data directory rather than an upgrade.
var ErrSchemaIncompatible = errors.New("database schema is incompatible; initialize a new data directory")

type Migration struct {
	Version int
	SQL     string
}

// CurrentSchemaVersion is the highest schema version produced by this binary.
const CurrentSchemaVersion = 21

// Open opens a persistent local database. The driver applies connection-local
// pragmas to every pooled connection, not just the migration connection.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	db, err := openDatabase(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := initializeMainSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenWithMigrations applies an independent schema using the same secure file
// checks and SQLite connection settings as the main database.
func OpenWithMigrations(ctx context.Context, path string, migrations []Migration) (*sql.DB, error) {
	db, err := openDatabase(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func openDatabase(ctx context.Context, path string) (*sql.DB, error) {
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
	return db, nil
}

func initializeMainSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var objects int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`).Scan(&objects); err != nil {
		return err
	}
	if objects == 0 {
		if _, err := tx.ExecContext(ctx, mainSchema+`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, CurrentSchemaVersion, timekey.Format(time.Now())); err != nil {
			return err
		}
		return tx.Commit()
	}
	var ledger int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_migrations'`).Scan(&ledger); err != nil {
		return err
	}
	if ledger != 1 {
		return ErrSchemaIncompatible
	}
	var count int
	var version sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),max(version) FROM schema_migrations`).Scan(&count, &version); err != nil {
		return fmt.Errorf("%w: %v", ErrSchemaIncompatible, err)
	}
	if version.Valid && version.Int64 > CurrentSchemaVersion {
		return ErrSchemaTooNew
	}
	if count != 1 || !version.Valid || version.Int64 != CurrentSchemaVersion {
		return ErrSchemaIncompatible
	}
	return tx.Commit()
}

const mainSchema = foundationSchema + alertsSchema + notificationsSchema + trafficSchema + probesSchema + backupsSchema

// OpenReader opens an existing database through a separate read-only pool.
// Open must run first so the database exists and its schema is initialized.
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

func applyMigration(ctx context.Context, db *sql.DB, migration Migration) error {
	tx, err := db.BeginTx(ctx, nil)
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
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)", migration.Version, timekey.Format(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

const foundationSchema = `
CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
 must_change_password INTEGER NOT NULL DEFAULT 0 CHECK(must_change_password IN (0,1)),
 created_at TEXT NOT NULL
);
CREATE TABLE sessions (
 token_hash BLOB PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_expiry ON sessions(expires_at);
CREATE TABLE groups (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE nodes (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
 group_id TEXT REFERENCES groups(id) ON DELETE SET NULL,
 region TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', notes TEXT NOT NULL DEFAULT '',
 token_hash BLOB UNIQUE, installation_id TEXT, first_enrolled_at TEXT, last_seen TEXT,
 credential_epoch INTEGER NOT NULL DEFAULT 0,
 system TEXT NOT NULL DEFAULT '{}', public_settings TEXT NOT NULL DEFAULT '{}',
 agent_runtime TEXT NOT NULL DEFAULT '', billing_day INTEGER NOT NULL DEFAULT 1,
 quota_bytes TEXT NOT NULL DEFAULT '0', created_at TEXT NOT NULL,
 run_generation TEXT NOT NULL DEFAULT '0', report_session_id TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX nodes_installation_identity ON nodes(installation_id) WHERE installation_id IS NOT NULL;
CREATE INDEX nodes_group ON nodes(group_id);
CREATE TABLE node_tags (node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, tag TEXT NOT NULL, PRIMARY KEY(node_id,tag));
CREATE TABLE enrollment_tokens (
 id TEXT PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE, node_id TEXT REFERENCES nodes(id) ON DELETE CASCADE,
 group_id TEXT REFERENCES groups(id) ON DELETE SET NULL, expires_at TEXT NOT NULL,
 max_uses INTEGER NOT NULL DEFAULT 1, uses INTEGER NOT NULL DEFAULT 0, revoked_at TEXT
);
CREATE TABLE enrollment_claims (
 enrollment_id TEXT NOT NULL REFERENCES enrollment_tokens(id) ON DELETE CASCADE,
 installation_id TEXT NOT NULL, node_id TEXT NOT NULL,
 token_hash BLOB NOT NULL, claimed_at TEXT NOT NULL, PRIMARY KEY(enrollment_id,installation_id)
);
CREATE INDEX enrollment_tokens_expiry_id ON enrollment_tokens(expires_at DESC, id);
CREATE INDEX enrollment_tokens_node ON enrollment_tokens(node_id);
CREATE INDEX enrollment_tokens_group ON enrollment_tokens(group_id);
CREATE INDEX enrollment_claims_node ON enrollment_claims(node_id);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE audit_events (
 id TEXT PRIMARY KEY, actor_id TEXT, action TEXT NOT NULL, target_id TEXT,
 created_at TEXT NOT NULL, details TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_events_created_id ON audit_events(created_at DESC, id DESC);
CREATE TABLE update_jobs (id TEXT PRIMARY KEY, release_seq INTEGER NOT NULL, created_at TEXT NOT NULL);
-- Node IDs deliberately survive asset deletion so cancellation remains observable.
CREATE TABLE update_job_nodes (
 job_id TEXT NOT NULL REFERENCES update_jobs(id) ON DELETE CASCADE,
 node_id TEXT NOT NULL, status TEXT NOT NULL, PRIMARY KEY(job_id,node_id)
);
CREATE INDEX update_job_nodes_node ON update_job_nodes(node_id);
`
