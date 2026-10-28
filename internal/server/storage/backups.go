package storage

// backupsSchema adds only data-operations state. Plan and actor references are
// snapshots, deliberately not cascading foreign keys into operational history.
const backupsSchema = `
CREATE TABLE backup_plans (
 id TEXT PRIMARY KEY, name TEXT NOT NULL,
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 kind TEXT NOT NULL CHECK(kind IN ('local','object')),
 categories_json TEXT NOT NULL, cron TEXT NOT NULL, destination_json TEXT NOT NULL,
 credentials_ciphertext BLOB, retain_count INTEGER,
 next_run_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX backup_plans_due ON backup_plans(enabled,next_run_at,id);
CREATE TABLE backup_tasks (
 id TEXT PRIMARY KEY,
 kind TEXT NOT NULL CHECK(kind IN ('backup_export','backup_local','backup_remote')),
 state TEXT NOT NULL CHECK(state IN ('queued','running','succeeded','failed')),
 phase TEXT NOT NULL DEFAULT '' CHECK(phase IN ('','snapshot','packaging','storing')),
 plan_id TEXT, actor_id TEXT, categories_json TEXT NOT NULL, destination_json TEXT NOT NULL,
 input_credentials_ciphertext BLOB, scheduled_at TEXT, created_at TEXT NOT NULL,
 started_at TEXT, finished_at TEXT, artifact_name TEXT, artifact_bytes INTEGER,
 artifact_sha256 TEXT, artifact_expires_at TEXT, artifact_deleted_at TEXT,
 artifact_cleanup_after TEXT, result_location TEXT, password_ciphertext BLOB,
 password_expires_at TEXT, error_code TEXT NOT NULL DEFAULT '',
 warning_code TEXT NOT NULL DEFAULT '', restored INTEGER NOT NULL DEFAULT 0 CHECK(restored IN (0,1)),
 UNIQUE(plan_id,scheduled_at)
);
CREATE UNIQUE INDEX backup_tasks_plan_active ON backup_tasks(plan_id)
 WHERE plan_id IS NOT NULL AND state IN ('queued','running');
CREATE INDEX backup_tasks_history ON backup_tasks(created_at DESC,id DESC);
CREATE INDEX backup_tasks_retention ON backup_tasks(finished_at,id) WHERE state IN ('succeeded','failed');
CREATE INDEX backup_tasks_password_expiry ON backup_tasks(password_expires_at,id)
 WHERE state IN ('succeeded','failed') AND password_ciphertext IS NOT NULL;
CREATE INDEX backup_tasks_artifact_cleanup ON backup_tasks(artifact_cleanup_after,id)
 WHERE kind='backup_export' AND artifact_deleted_at IS NULL AND restored=0;
CREATE TABLE inbox_notifications (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('task','alert')), source_id TEXT NOT NULL,
 transition TEXT NOT NULL, audience TEXT NOT NULL CHECK(audience IN ('admin','authenticated')),
 severity TEXT NOT NULL, created_at TEXT NOT NULL, details_json TEXT NOT NULL,
 UNIQUE(kind,source_id,transition)
);
CREATE INDEX inbox_notifications_history ON inbox_notifications(created_at DESC,id DESC);
CREATE INDEX inbox_notifications_audience ON inbox_notifications(audience,created_at,id);
CREATE TABLE inbox_reads (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 notification_id TEXT NOT NULL REFERENCES inbox_notifications(id) ON DELETE CASCADE,
 read_at TEXT NOT NULL, PRIMARY KEY(user_id,notification_id)
);
`
