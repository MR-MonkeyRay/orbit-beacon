package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyAgentCommandsSchema = `CREATE TABLE agent_commands (
 id TEXT PRIMARY KEY NOT NULL, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 credential_epoch INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('upgrade','restart','stop')),
 target_version TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('pending','delivered','succeeded','failed','rejected','expired','canceled')),
 result_code TEXT NOT NULL DEFAULT '' CHECK(result_code IN ('','ok','disabled_locally','unsupported','expired','interrupted','failed','download','checksum','self_check','downgrade','rolled_back')),
 requested_by TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
 delivered_at TEXT, delivered_generation TEXT NOT NULL DEFAULT '', finished_at TEXT,
 CHECK ((kind='upgrade' AND length(target_version) BETWEEN 1 AND 64) OR (kind IN ('restart','stop') AND target_version=''))
);
CREATE UNIQUE INDEX agent_commands_active ON agent_commands(node_id) WHERE state IN ('pending','delivered');
CREATE INDEX agent_commands_recent ON agent_commands(node_id,created_at DESC,id DESC);
CREATE INDEX agent_commands_finished ON agent_commands(finished_at) WHERE state NOT IN ('pending','delivered');
`

func legacyCommandsDatabase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legacy.db")
	db, err := openDatabase(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	start, end := strings.Index(mainSchema, "CREATE TABLE agent_commands ("), strings.Index(mainSchema, "CREATE TABLE node_tags (")
	legacy := mainSchema[:start] + legacyAgentCommandsSchema + mainSchema[end:]
	if err := initializeSQLiteSchema(t.Context(), db, legacy, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,totp_ciphertext,created_at) VALUES('user','operator','hash',X'0001FF','original'); INSERT INTO settings(key,value) VALUES('preserved','original');`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "delivered", "succeeded", "failed", "rejected", "expired", "canceled"} {
		if _, err := db.Exec(`INSERT INTO nodes(id,name,created_at) VALUES(?,?, 'original');`, state, state); err != nil {
			t.Fatal(err)
		}
		var finished any = "2030-01-01T00:02:00.000000000Z"
		if state == "pending" || state == "delivered" {
			finished = nil
		}
		if _, err := db.Exec(`INSERT INTO agent_commands(id,node_id,credential_epoch,kind,target_version,state,result_code,requested_by,created_at,expires_at,delivered_at,delivered_generation,finished_at) VALUES(?,?,3,'upgrade','1.2.4',?,'','operator','2030-01-01T00:00:00.000000000Z','2030-01-01T00:20:00.000000000Z','2030-01-01T00:01:00.000000000Z','7',?)`, state, state, state, finished); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at) VALUES('restart','succeeded',3,'restart','pending','operator','original','later')`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandSchemaMigratesVersionTwoWithoutLosingData(t *testing.T) {
	path := legacyCommandsDatabase(t)
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != 3 {
		t.Fatal(version, err)
	}
	var user, secret, setting string
	if err := db.QueryRow(`SELECT username,hex(totp_ciphertext),(SELECT value FROM settings WHERE key='preserved') FROM users WHERE id='user'`).Scan(&user, &secret, &setting); err != nil || user != "operator" || secret != "0001FF" || setting != "original" {
		t.Fatal(user, secret, setting, err)
	}
	for _, old := range []string{"pending", "delivered", "succeeded", "failed", "rejected", "expired", "canceled"} {
		var state, info, delivered, generation string
		var finished *string
		var epoch int64
		if err := db.QueryRow(`SELECT state,update_info,credential_epoch,delivered_at,delivered_generation,finished_at FROM agent_commands WHERE id=?`, old).Scan(&state, &info, &epoch, &delivered, &generation, &finished); err != nil {
			t.Fatal(err)
		}
		want := old
		if old == "pending" || old == "delivered" {
			want = "canceled"
		}
		var metadata map[string]string
		if json.Unmarshal([]byte(info), &metadata) != nil || metadata["latest_version"] != "1.2.4" || state != want || epoch != 3 || generation != "7" || finished == nil || delivered != "2030-01-01T00:01:00.000000000Z" {
			t.Fatal(old, state, info, epoch, delivered, generation, finished)
		}
	}
	var restart, info string
	if err := db.QueryRow(`SELECT state,update_info FROM agent_commands WHERE id='restart'`).Scan(&restart, &info); err != nil || restart != "pending" || info != "null" {
		t.Fatal(restart, info, err)
	}
	if _, err := db.Exec(`INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at) VALUES('check','pending',3,'check_update','pending','operator','original','later')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}

func TestCommandSchemaMigrationRejectsReplicatedVersionTwoWithoutMutation(t *testing.T) {
	path := legacyCommandsDatabase(t)
	db, err := openDatabase(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE replica_identity(id INTEGER PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if opened, err := Open(t.Context(), path); opened != nil || !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatal(opened, err)
	}
	db, err = openDatabase(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var state, target string
	if err := db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != 2 {
		t.Fatal(version, err)
	}
	if err := db.QueryRow(`SELECT state,target_version FROM agent_commands WHERE id='pending'`).Scan(&state, &target); err != nil || state != "pending" || target != "1.2.4" {
		t.Fatal(state, target, err)
	}
}

func TestCurrentCommandSchemaRejectsLegacyTableAndPreservesLedger(t *testing.T) {
	path := legacyCommandsDatabase(t)
	db, err := openDatabase(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET version=3`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if opened, err := Open(t.Context(), path); opened != nil || !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatal(opened, err)
	}
}
