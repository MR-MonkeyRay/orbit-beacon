package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestPayloadSchemaRejectsExecutableAndExtraObjects(t *testing.T) {
	for _, mutation := range []string{
		`CREATE VIEW unexpected AS SELECT id FROM users`,
		`CREATE TRIGGER unexpected AFTER INSERT ON users BEGIN DELETE FROM users; END`,
		`CREATE TABLE unexpected(secret TEXT)`,
		`CREATE INDEX unexpected ON users(username)`,
		`ALTER TABLE users ADD COLUMN extra TEXT`,
		`DROP TABLE users; CREATE TABLE users (id TEXT,username TEXT,password_hash TEXT,created_at TEXT,must_change_password INTEGER,PRIMARY KEY(id),CHECK(length(username)<100))`,
		`DROP TABLE users; CREATE TABLE users (id TEXT,username TEXT COLLATE NOCASE,password_hash TEXT,created_at TEXT,must_change_password INTEGER,PRIMARY KEY(id))`,
		`DROP TABLE users; CREATE TABLE users (id TEXT,username TEXT,password_hash TEXT,created_at TEXT,must_change_password INTEGER,PRIMARY KEY(id)) WITHOUT ROWID`,
	} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "payload.sqlite")
			db, err := createPayload(context.Background(), path, Configuration)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadSchema(context.Background(), db, Configuration); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadSchema(context.Background(), db, Configuration); err == nil {
				t.Fatal("unsafe payload accepted")
			}
			db.Close()
		})
	}
}

func TestPayloadSchemaRejectsRemoteConfigurationColumns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		category Category
		mutation string
	}{
		{"group desired configuration", Configuration, `ALTER TABLE groups ADD COLUMN desired_config TEXT`},
		{"node desired revision", Configuration, `ALTER TABLE nodes ADD COLUMN desired_revision INTEGER`},
		{"node runtime configuration", Configuration, `ALTER TABLE nodes ADD COLUMN report_interval_seconds INTEGER`},
		{"missing credential identity epoch", Configuration, `ALTER TABLE nodes DROP COLUMN credential_epoch`},
		{"credential identity in monitoring", Monitoring, `ALTER TABLE node_observations ADD COLUMN credential_epoch INTEGER`},
		{"observation applied revision", Monitoring, `DROP TABLE node_observations; CREATE TABLE node_observations (node_id TEXT,last_seen TEXT,system TEXT,applied_revision INTEGER,config_error TEXT,PRIMARY KEY(node_id))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			db, err := createPayload(ctx, filepath.Join(dir, "payload.sqlite"), tc.category)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec(tc.mutation); err != nil {
				t.Fatal(err)
			}
			if err = ValidatePayloadSchema(ctx, db, tc.category); err == nil {
				t.Fatal("accepted obsolete remote configuration schema")
			}
		})
	}
}

func TestSnapshotIncludesCommittedWALWithoutOwningServerLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "orbitbeacon.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`PRAGMA wal_autocheckpoint=0; INSERT INTO settings VALUES('sentinel','committed')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE settings SET value='uncommitted' WHERE key='sentinel'`); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "snapshot.db")
	if err = SnapshotDatabase(ctx, path, output, storage.CurrentSchemaVersion); err != nil {
		t.Fatal(err)
	}
	reader, err := openPayload(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var got string
	if err = reader.QueryRow(`SELECT value FROM settings WHERE key='sentinel'`).Scan(&got); err != nil || got != "committed" {
		t.Fatalf("snapshot=%q err=%v", got, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE settings SET value='after' WHERE key='sentinel'`); err != nil {
		t.Fatal(err)
	}
	if err = reader.QueryRow(`SELECT value FROM settings WHERE key='sentinel'`).Scan(&got); err != nil || got != "committed" {
		t.Fatalf("snapshot changed=%q err=%v", got, err)
	}
}

func TestPayloadRejectsGeneratedAndVirtualTables(t *testing.T) {
	for _, ddl := range []string{
		`CREATE TABLE users(id TEXT PRIMARY KEY,username TEXT,password_hash TEXT,created_at TEXT,must_change_password INTEGER,extra TEXT GENERATED ALWAYS AS(username) VIRTUAL)`,
		`CREATE VIRTUAL TABLE users USING fts5(id,username,password_hash,created_at,must_change_password)`,
	} {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bad.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		if err = ValidatePayloadSchema(context.Background(), db, Configuration); err == nil {
			t.Fatal("malicious schema accepted")
		}
		db.Close()
	}
}

func TestSnapshotNeverOverwritesExistingOutput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "orbitbeacon.db")
	db, err := storage.Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	output := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(output, []byte("preserve existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SnapshotDatabase(ctx, source, output, storage.CurrentSchemaVersion); err == nil {
		t.Fatal("existing snapshot overwritten")
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != "preserve existing" {
		t.Fatalf("existing file=%q err=%v", got, err)
	}
	actual := filepath.Join(dir, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if err := SnapshotDatabase(ctx, source, filepath.Join(link, "snapshot.db"), storage.CurrentSchemaVersion); err == nil {
		t.Fatal("snapshot followed symlink ancestor")
	}
}

func TestPayloadIntegrityRejectsCorruptedSQLite(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "corrupted.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ValidateSQLite(context.Background(), db); err == nil {
		t.Fatal("corrupt SQLite accepted")
	}
}
