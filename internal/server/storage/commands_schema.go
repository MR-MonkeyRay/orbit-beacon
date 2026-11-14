package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"orbitbeacon/internal/timekey"
)

const agentCommandsSchema = `CREATE TABLE agent_commands (
 id TEXT PRIMARY KEY NOT NULL, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 credential_epoch INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('check_update','upgrade','restart','stop')),
 update_info TEXT NOT NULL DEFAULT 'null' CHECK(json_valid(update_info)),
 state TEXT NOT NULL CHECK(state IN ('pending','delivered','succeeded','failed','rejected','expired','canceled')),
 result_code TEXT NOT NULL DEFAULT '' CHECK(result_code IN ('','ok','disabled_locally','unsupported','expired','interrupted','failed','download','checksum','self_check','downgrade','rolled_back')),
 requested_by TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
 delivered_at TEXT, delivered_generation TEXT NOT NULL DEFAULT '', finished_at TEXT
);
`

const mysqlAgentCommandsSchema = "CREATE TABLE `agent_commands` (\n" +
	" `id` VARBINARY(64) PRIMARY KEY,\n `node_id` VARBINARY(64) NOT NULL,\n `credential_epoch` BIGINT NOT NULL,\n" +
	" `kind` VARCHAR(255) NOT NULL CHECK(kind IN ('check_update','upgrade','restart','stop')),\n" +
	" `update_info` LONGTEXT NOT NULL DEFAULT ('null') CHECK(JSON_VALID(update_info)),\n" +
	" `state` VARCHAR(255) NOT NULL CHECK(state IN ('pending','delivered','succeeded','failed','rejected','expired','canceled')),\n" +
	" `result_code` VARCHAR(255) NOT NULL DEFAULT '' CHECK(result_code IN ('','ok','disabled_locally','unsupported','expired','interrupted','failed','download','checksum','self_check','downgrade','rolled_back')),\n" +
	" `requested_by` VARBINARY(64) NOT NULL,\n `created_at` VARCHAR(255) NOT NULL,\n `expires_at` VARCHAR(255) NOT NULL,\n" +
	" `delivered_at` VARCHAR(255),\n `delivered_generation` LONGTEXT NOT NULL DEFAULT (''),\n `finished_at` VARCHAR(255),\n" +
	" FOREIGN KEY (`node_id`) REFERENCES `nodes`(`id`) ON DELETE CASCADE,\n" +
	" active_node VARBINARY(64) GENERATED ALWAYS AS (CASE WHEN state IN ('pending','delivered') THEN node_id END) VIRTUAL,\n" +
	" UNIQUE KEY agent_commands_active(active_node)\n) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;"

const commandIndexes = `CREATE UNIQUE INDEX agent_commands_active ON agent_commands(node_id) WHERE state IN ('pending','delivered');
CREATE INDEX agent_commands_recent ON agent_commands(node_id,created_at DESC,id DESC);
CREATE INDEX agent_commands_finished ON agent_commands(finished_at) WHERE state NOT IN ('pending','delivered');`

type schemaQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateMainColumns(ctx context.Context, q schemaQuerier, legacy bool) error {
	for _, table := range Tables(MainDomain) {
		columns := columnNames(table.Columns)
		if legacy && table.Name == "agent_commands" {
			columns = strings.Replace(columns, "update_info", "target_version", 1)
		}
		rows, err := q.QueryContext(ctx, "SELECT "+columns+" FROM `"+table.Name+"` LIMIT 0")
		if err != nil {
			return ErrSchemaIncompatible
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func commandColumnShape(ctx context.Context, q schemaQuerier, mysql bool) (string, error) {
	rows, err := q.QueryContext(ctx, "SELECT * FROM agent_commands LIMIT 0")
	if err != nil {
		return "", ErrSchemaIncompatible
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if mysql && len(columns) > 0 && columns[len(columns)-1] == "active_node" {
		columns = columns[:len(columns)-1]
	}
	actual := strings.Join(columns, ",")
	for _, table := range Tables(MainDomain) {
		if table.Name != "agent_commands" {
			continue
		}
		expected := make([]string, len(table.Columns))
		for i, column := range table.Columns {
			expected[i] = column.Name
		}
		current := strings.Join(expected, ",")
		switch actual {
		case current:
			return "current", nil
		case strings.Replace(current, "update_info", "target_version", 1):
			return "legacy", nil
		case strings.Replace(current, "update_info", "update_info,target_version", 1):
			return "staged", nil
		}
	}
	return "", ErrSchemaIncompatible
}

func normalizedCommandDDL(s string) string {
	s = strings.NewReplacer("`", "", "\"", "", ";", "").Replace(s)
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func validateSQLiteCommandSchema(ctx context.Context, tx *sql.Tx) error {
	var ddl string
	if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE type='table' AND name='agent_commands'").Scan(&ddl); err != nil || normalizedCommandDDL(ddl) != normalizedCommandDDL(agentCommandsSchema) {
		return ErrSchemaIncompatible
	}
	return nil
}

func migrateSQLiteCommands(ctx context.Context, tx *sql.Tx) error {
	// 旧复制日志按列位置编码；不能把 target_version 的历史日志解释为 update_info。
	var replicas, triggers int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name GLOB 'replica_*' OR name GLOB 'ob_replica_*'").Scan(&replicas); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND tbl_name='agent_commands'").Scan(&triggers); err != nil {
		return err
	}
	if replicas != 0 || triggers != 0 {
		return fmt.Errorf("%w: schema-2 replication requires offline reseeding", ErrSchemaIncompatible)
	}
	if err := validateMainColumns(ctx, tx, true); err != nil {
		return err
	}
	if shape, err := commandColumnShape(ctx, tx, false); err != nil || shape != "legacy" {
		return ErrSchemaIncompatible
	}
	create := strings.Replace(agentCommandsSchema, "CREATE TABLE agent_commands", "CREATE TABLE agent_commands_v3", 1)
	if _, err := tx.ExecContext(ctx, create); err != nil {
		return err
	}
	now := timekey.Format(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_commands_v3
 SELECT id,node_id,credential_epoch,kind,
 CASE WHEN target_version='' THEN 'null' ELSE json_object('latest_version',target_version) END,
 CASE WHEN kind='upgrade' AND state IN ('pending','delivered') THEN 'canceled' ELSE state END,
 result_code,requested_by,created_at,expires_at,delivered_at,delivered_generation,
 CASE WHEN kind='upgrade' AND state IN ('pending','delivered') THEN COALESCE(finished_at,?) ELSE finished_at END
 FROM agent_commands`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE agent_commands; ALTER TABLE agent_commands_v3 RENAME TO agent_commands;`+commandIndexes); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE schema_migrations SET version=?,applied_at=? WHERE version=2", CurrentSchemaVersion, now)
	return err
}

func validateMySQLCommandSchema(ctx context.Context, db *sql.DB) error {
	if shape, err := commandColumnShape(ctx, db, true); err != nil || shape != "current" {
		return ErrSchemaIncompatible
	}
	var ddl, table string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE agent_commands").Scan(&table, &ddl); err != nil {
		return ErrSchemaIncompatible
	}
	ddl = strings.ToLower(ddl)
	if !strings.Contains(ddl, "'check_update'") || !strings.Contains(ddl, "json_valid(`update_info`)") || strings.Contains(ddl, "target_version") {
		return ErrSchemaIncompatible
	}
	return nil
}

func migrateMySQLCommands(ctx context.Context, db *sql.DB) error {
	var replicas, triggers int
	var guard int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM storage_write_guard WHERE id=1").Scan(&guard); err != nil || guard != 1 {
		return ErrSchemaIncompatible
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND LEFT(table_name,8)='replica_'").Scan(&replicas); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.triggers WHERE trigger_schema=DATABASE() AND event_object_table='agent_commands'").Scan(&triggers); err != nil {
		return err
	}
	if replicas != 0 || triggers != 0 {
		return fmt.Errorf("%w: schema-2 replication requires offline reseeding", ErrSchemaIncompatible)
	}
	shape, err := commandColumnShape(ctx, db, true)
	if err != nil {
		return err
	}
	if err := validateMainColumns(ctx, db, shape != "current"); err != nil {
		return err
	}
	if shape == "legacy" {
		rows, err := db.QueryContext(ctx, `SELECT c.CONSTRAINT_NAME,c.CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS c JOIN information_schema.TABLE_CONSTRAINTS t ON t.CONSTRAINT_SCHEMA=c.CONSTRAINT_SCHEMA AND t.CONSTRAINT_NAME=c.CONSTRAINT_NAME WHERE t.TABLE_SCHEMA=DATABASE() AND t.TABLE_NAME='agent_commands' AND t.CONSTRAINT_TYPE='CHECK'`)
		if err != nil {
			return err
		}
		var drops []string
		for rows.Next() {
			var name, clause string
			if err := rows.Scan(&name, &clause); err != nil {
				rows.Close()
				return err
			}
			if strings.Contains(strings.ToLower(clause), "kind") || strings.Contains(strings.ToLower(clause), "target_version") {
				drops = append(drops, "DROP CHECK `"+strings.ReplaceAll(name, "`", "``")+"`")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(drops) != 2 {
			return ErrSchemaIncompatible
		}
		// 两次原子 ALTER 之间保留旧列；DDL 中断后可从列形状继续，不丢历史目标。
		alter := "ALTER TABLE agent_commands " + strings.Join(drops, ",") + ", ADD COLUMN update_info LONGTEXT NOT NULL DEFAULT ('null') CHECK(JSON_VALID(update_info)) AFTER kind, ADD CONSTRAINT agent_commands_kind_v3 CHECK(kind IN ('check_update','upgrade','restart','stop'))"
		if _, err := db.ExecContext(ctx, alter); err != nil {
			return err
		}
		shape = "staged"
	}
	if shape == "staged" {
		if _, err := db.ExecContext(ctx, `UPDATE agent_commands SET update_info=CASE WHEN target_version='' THEN 'null' ELSE JSON_OBJECT('latest_version',target_version) END,
 finished_at=CASE WHEN kind='upgrade' AND state IN ('pending','delivered') THEN COALESCE(finished_at,?) ELSE finished_at END,
 state=CASE WHEN kind='upgrade' AND state IN ('pending','delivered') THEN 'canceled' ELSE state END`, timekey.Format(time.Now())); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "ALTER TABLE agent_commands DROP COLUMN target_version"); err != nil {
			return err
		}
	}
	if err := validateMySQLCommandSchema(ctx, db); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE schema_migrations SET version=?,applied_at=? WHERE version=2", CurrentSchemaVersion, timekey.Format(time.Now()))
	return err
}
