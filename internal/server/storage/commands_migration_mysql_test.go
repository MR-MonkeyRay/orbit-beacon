package storage_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

const legacyMySQLCommands = `CREATE TABLE agent_commands (
 id VARBINARY(64) PRIMARY KEY, node_id VARBINARY(64) NOT NULL,
 credential_epoch BIGINT NOT NULL,
 kind VARCHAR(255) NOT NULL CHECK(kind IN ('upgrade','restart','stop')),
 target_version VARCHAR(255) NOT NULL DEFAULT '',
 state VARCHAR(255) NOT NULL CHECK(state IN ('pending','delivered','succeeded','failed','rejected','expired','canceled')),
 result_code VARCHAR(255) NOT NULL DEFAULT '' CHECK(result_code IN ('','ok','disabled_locally','unsupported','expired','interrupted','failed','download','checksum','self_check','downgrade','rolled_back')),
 requested_by VARBINARY(64) NOT NULL, created_at VARCHAR(255) NOT NULL, expires_at VARCHAR(255) NOT NULL,
 delivered_at VARCHAR(255), delivered_generation LONGTEXT NOT NULL DEFAULT (''), finished_at VARCHAR(255),
 CHECK ((kind='upgrade' AND char_length(target_version) BETWEEN 1 AND 64) OR (kind IN ('restart','stop') AND target_version='')),
 FOREIGN KEY(node_id) REFERENCES nodes(id) ON DELETE CASCADE,
 active_node VARBINARY(64) GENERATED ALWAYS AS (CASE WHEN state IN ('pending','delivered') THEN node_id END) VIRTUAL,
 UNIQUE KEY agent_commands_active(active_node)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`

func TestMySQLCommandSchemaVersionTwoMigration(t *testing.T) {
	for _, phase := range []string{"legacy", "staged", "converted"} {
		t.Run(phase, func(t *testing.T) {
			f := storagetest.Start(t)
			raw := f.Raw(t, storage.MainDomain)
			for _, statement := range []string{
				"DROP TABLE agent_commands", legacyMySQLCommands,
				"CREATE INDEX agent_commands_recent ON agent_commands(node_id,created_at DESC,id DESC)",
				"CREATE INDEX agent_commands_finished ON agent_commands(finished_at)",
				"UPDATE schema_migrations SET version=2",
				"INSERT INTO nodes(id,name,created_at) VALUES('node','preserved','original')",
				"INSERT INTO agent_commands(id,node_id,credential_epoch,kind,target_version,state,requested_by,created_at,expires_at) VALUES('pending','node',7,'upgrade','1.2.4','pending','operator','original','later'),('history','node',7,'upgrade','1.2.3','succeeded','operator','original','later')",
			} {
				if _, err := raw.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if phase != "legacy" {
				rows, err := raw.Query(`SELECT c.CONSTRAINT_NAME,c.CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS c JOIN information_schema.TABLE_CONSTRAINTS t ON t.CONSTRAINT_SCHEMA=c.CONSTRAINT_SCHEMA AND t.CONSTRAINT_NAME=c.CONSTRAINT_NAME WHERE t.TABLE_SCHEMA=DATABASE() AND t.TABLE_NAME='agent_commands' AND t.CONSTRAINT_TYPE='CHECK'`)
				if err != nil {
					t.Fatal(err)
				}
				var drops []string
				for rows.Next() {
					var name, clause string
					if err := rows.Scan(&name, &clause); err != nil {
						t.Fatal(err)
					}
					// Fixture constraint names are MySQL-generated identifiers.
					if strings.Contains(clause, "kind") || strings.Contains(clause, "target_version") {
						drops = append(drops, "DROP CHECK `"+name+"`")
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				rows.Close()
				if len(drops) != 2 {
					t.Fatal(drops)
				}
				if _, err := raw.Exec("ALTER TABLE agent_commands " + drops[0] + "," + drops[1] + ", ADD COLUMN update_info LONGTEXT NOT NULL DEFAULT ('null') CHECK(JSON_VALID(update_info)) AFTER kind, ADD CONSTRAINT agent_commands_kind_v3 CHECK(kind IN ('check_update','upgrade','restart','stop'))"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "converted" {
				if _, err := raw.Exec(`UPDATE agent_commands SET update_info=JSON_OBJECT('latest_version',target_version),finished_at=CASE WHEN state='pending' THEN 'migration' ELSE finished_at END,state=CASE WHEN state='pending' THEN 'canceled' ELSE state END`); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec("ALTER TABLE agent_commands DROP COLUMN target_version"); err != nil {
					t.Fatal(err)
				}
			}
			db, err := storage.OpenDomain(t.Context(), f.Endpoint("primary"), storage.MainDomain, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			for _, id := range []string{"pending", "history"} {
				var state, info string
				if err := raw.QueryRow("SELECT state,update_info FROM agent_commands WHERE id=?", id).Scan(&state, &info); err != nil {
					t.Fatal(err)
				}
				var metadata map[string]string
				if json.Unmarshal([]byte(info), &metadata) != nil || metadata["latest_version"] == "" || (id == "pending" && state != "canceled") || (id == "history" && state != "succeeded") {
					t.Fatal(id, state, info)
				}
			}
			if _, err := raw.Exec("INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at) VALUES('check','node',7,'check_update','pending','operator','original','later')"); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("UPDATE schema_migrations SET version=2"); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("CREATE TABLE replica_identity(id BIGINT PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
			if opened, err := storage.OpenDomain(t.Context(), f.Endpoint("primary"), storage.MainDomain, t.TempDir()); opened != nil || !errors.Is(err, storage.ErrSchemaIncompatible) {
				t.Fatal(opened, err)
			}
		})
	}
}
