package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const probesSchema = `
CREATE TABLE probe_latest (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 target TEXT NOT NULL,
 ok INTEGER NOT NULL CHECK(ok IN (0,1)),
 latency_ms REAL CHECK(latency_ms IS NULL OR latency_ms>=0),
 error TEXT NOT NULL CHECK(error IN ('','timeout','refused','dns','tls','http_status','unreachable','unsupported','error')),
 status_code INTEGER CHECK(status_code IS NULL OR status_code BETWEEN 100 AND 599),
 checked_at TEXT NOT NULL,
 PRIMARY KEY(node_id,target)
) WITHOUT ROWID;
CREATE TABLE probe_hourly (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 target TEXT NOT NULL,
 hour TEXT NOT NULL,
 checks INTEGER NOT NULL CHECK(checks>=0),
 failures INTEGER NOT NULL CHECK(failures>=0 AND failures<=checks),
 latency_sum_ms REAL NOT NULL CHECK(latency_sum_ms>=0),
 PRIMARY KEY(node_id,target,hour)
) WITHOUT ROWID;
CREATE INDEX probe_hourly_retention ON probe_hourly(hour);
`

// Keep migration 10 immutable. Migration 14 rebuilds its table with the expanded
// metric CHECK. The probe threshold bound remains in Go rule validation. Scope
// columns remain VIRTUAL foreign keys, and are recomputed rather than inserted.
const probeAlertRulesSchema = `
CREATE TABLE alert_rules_probes (
 id TEXT PRIMARY KEY, name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 scope_kind TEXT NOT NULL CHECK(scope_kind IN ('all','group','node')), scope_id TEXT,
 scope_group_id TEXT GENERATED ALWAYS AS (CASE WHEN scope_kind='group' THEN scope_id END) VIRTUAL REFERENCES groups(id) ON DELETE CASCADE,
 scope_node_id TEXT GENERATED ALWAYS AS (CASE WHEN scope_kind='node' THEN scope_id END) VIRTUAL REFERENCES nodes(id) ON DELETE CASCADE,
 metric TEXT NOT NULL CHECK(metric IN ('cpu_percent','memory_used_percent','swap_used_percent','disk_used_percent','load1','offline','probes_down')),
 comparator TEXT, threshold REAL, for_seconds INTEGER NOT NULL CHECK(for_seconds BETWEEN 0 AND 86400),
 severity TEXT NOT NULL CHECK(severity IN ('info','warning','critical')),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 CHECK((scope_kind='all' AND scope_id IS NULL) OR (scope_kind IN ('group','node') AND scope_id IS NOT NULL)),
 CHECK((metric='offline' AND comparator IS NULL AND threshold IS NULL) OR
       (metric!='offline' AND comparator IS NOT NULL AND comparator IN ('>','<') AND threshold IS NOT NULL AND threshold>=0 AND
        ((metric='load1' AND threshold<=1000000) OR (metric!='load1' AND threshold<=100))))
);
`

// rebuildProbeAlertRules follows SQLite's generalized ALTER TABLE procedure:
// https://sqlite.org/lang_altertable.html#making_other_kinds_of_table_schema_changes
// with foreign keys disabled/restored around this transaction by applyMigration.
// Create/copy/drop/rename avoids rewriting child FKs to a temporary table name.
func rebuildProbeAlertRules(ctx context.Context, tx *sql.Tx) error {
	type schemaObject struct{ kind, name, sql string }
	// Preserve exact index/trigger/view SQL. Temporarily removing all views and
	// triggers also covers indirect dependencies through other views or tables;
	// otherwise modern ALTER TABLE can reject the temporarily missing parent.
	rows, err := tx.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_schema WHERE sql IS NOT NULL AND ((type='index' AND tbl_name='alert_rules') OR type IN ('trigger','view')) ORDER BY rowid`)
	if err != nil {
		return err
	}
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.sql); err != nil {
			_ = rows.Close()
			return err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, object := range objects {
		if object.kind == "index" {
			continue // Dropping the old table will drop its indexes.
		}
		name := `"` + strings.ReplaceAll(object.name, `"`, `""`) + `"`
		if _, err := tx.ExecContext(ctx, "DROP "+strings.ToUpper(object.kind)+" "+name); err != nil {
			return fmt.Errorf("detach %s %q: %w", object.kind, object.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, probeAlertRulesSchema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO alert_rules_probes(rowid,id,name,enabled,scope_kind,scope_id,metric,comparator,threshold,for_seconds,severity,created_at,updated_at)
 SELECT rowid,id,name,enabled,scope_kind,scope_id,metric,comparator,threshold,for_seconds,severity,created_at,updated_at FROM alert_rules`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE alert_rules; ALTER TABLE alert_rules_probes RENAME TO alert_rules;`); err != nil {
		return err
	}
	for _, object := range objects {
		if _, err := tx.ExecContext(ctx, object.sql); err != nil {
			return fmt.Errorf("restore %s %q: %w", object.kind, object.name, err)
		}
	}
	return nil
}
