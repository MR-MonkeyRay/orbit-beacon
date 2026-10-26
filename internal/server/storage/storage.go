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
)

type Migration struct {
	Version int
	SQL     string
	Apply   func(context.Context, *sql.Tx) error
}

// Open opens a persistent local database. The driver applies connection-local
// pragmas to every pooled connection, not just the migration connection.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := config.ValidatePath(path); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := validateLocalFilesystem(absolute); err != nil {
		return nil, err
	}
	// Inspect every existing component before creating even an empty database.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := securefs.ValidateFile(absolute + suffix); err != nil {
			return nil, err
		}
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
	if err := Migrate(ctx, db, []Migration{{Version: 1, SQL: foundationSchema}, {Version: 2, SQL: sessionsExpirySchema}, {Version: 3, SQL: reportIdentitySchema}, {Version: 4, SQL: enrollmentTombstoneSchema}, {Version: 5, Apply: repairReportSelections}, {Version: 6, SQL: recoverableCSRFSchema}, {Version: 7, SQL: auditOrderingSchema}}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate atomically applies each unapplied version together with its ledger row.
func Migrate(ctx context.Context, db *sql.DB, migrations []Migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
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
	if migration.Apply != nil {
		if err := migration.Apply(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)", migration.Version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
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

const recoverableCSRFSchema = `ALTER TABLE sessions DROP COLUMN csrf_hash;`
