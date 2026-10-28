package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func probeMigrationsThroughThirteen() []Migration {
	return []Migration{
		{Version: 1, SQL: foundationSchema}, {Version: 2, SQL: sessionsExpirySchema},
		{Version: 3, SQL: reportIdentitySchema}, {Version: 4, SQL: enrollmentTombstoneSchema},
		{Version: 5, Apply: repairReportSelections}, {Version: 6, SQL: recoverableCSRFSchema},
		{Version: 7, SQL: auditOrderingSchema}, {Version: 8, SQL: enrollmentOrderingSchema},
		{Version: 9, Apply: normalizeTimestampColumns}, {Version: 10, SQL: alertsSchema},
		{Version: 11, SQL: notificationsSchema}, {Version: 12, SQL: apiTokensSchema},
		{Version: 13, SQL: trafficSchema},
	}
}

func openProbeMigrationThirteen(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "v13.db")
	db, err := OpenWithMigrations(context.Background(), path, probeMigrationsThroughThirteen())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`
INSERT INTO groups(id,name) VALUES('group','retained group');
INSERT INTO nodes(id,name,created_at) VALUES('node','retained node','created');
INSERT INTO alert_rules(id,name,enabled,scope_kind,scope_id,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES
 ('all-rule','all',0,'all',NULL,'cpu_percent','>',80.5,25,'warning','all-created','all-updated'),
 ('group-rule','group',1,'group','group','offline',NULL,NULL,180,'critical','group-created','group-updated'),
 ('node-rule','node',1,'node','node','load1','<',10.5,60,'info','node-created','node-updated');
INSERT INTO notification_channels(id,name,kind,enabled,config_json,created_at,updated_at)
 VALUES('channel','channel','webhook',1,'{}','created','updated');
INSERT INTO alert_rule_channels SELECT id,'channel' FROM alert_rules;
CREATE TABLE probe_rule_children (id TEXT PRIMARY KEY, rule_id TEXT NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE, payload TEXT NOT NULL);
INSERT INTO probe_rule_children SELECT id,id,'retained payload' FROM alert_rules;
CREATE INDEX probe_rule_name_index ON alert_rules(name) WHERE enabled=1;
CREATE TABLE probe_rule_updates (rule_id TEXT NOT NULL);
CREATE TRIGGER probe_rule_update AFTER UPDATE OF name ON alert_rules BEGIN INSERT INTO probe_rule_updates VALUES(NEW.id); END;
CREATE VIEW probe_rule_view AS SELECT id,name,metric FROM alert_rules WHERE enabled=1;
`); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func probeMigrationSnapshot(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var snapshot [][]any
	for rows.Next() {
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertProbeMigrationForeignKeys(t *testing.T, db *sql.DB) {
	t.Helper()
	var enabled int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign_keys=%d err=%v", enabled, err)
	}
	if violations := probeMigrationSnapshot(t, db, `PRAGMA foreign_key_check`); len(violations) != 0 {
		t.Fatalf("foreign key violations: %v", violations)
	}
}

func TestProbeMigrationFromThirteenPreservesAlertRulesAndDependencies(t *testing.T) {
	db, path := openProbeMigrationThirteen(t)
	queries := []string{
		`SELECT rowid,* FROM alert_rules ORDER BY id`,
		`SELECT type,name,sql FROM sqlite_schema WHERE (tbl_name='alert_rules' AND type IN ('index','trigger')) OR name='probe_rule_view' ORDER BY type,name`,
		`SELECT * FROM alert_rule_channels ORDER BY rule_id,channel_id`,
		`SELECT * FROM probe_rule_children ORDER BY id`,
		`PRAGMA table_xinfo(alert_rules)`,
		`PRAGMA foreign_key_list(alert_rules)`,
		`PRAGMA foreign_key_list(alert_rule_channels)`,
		`PRAGMA foreign_key_list(probe_rule_children)`,
	}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = probeMigrationSnapshot(t, db, query)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for i, query := range queries {
		if after := probeMigrationSnapshot(t, db, query); !reflect.DeepEqual(before[i], after) {
			t.Errorf("migration changed %s: before=%v after=%v", query, before[i], after)
		}
	}
	for _, object := range []string{"alert_rules_group", "alert_rules_node", "probe_rule_name_index", "probe_rule_update", "probe_rule_view"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name=? AND sql IS NOT NULL`, object).Scan(&count); err != nil || count != 1 {
			t.Fatalf("schema object %s count=%d err=%v", object, count, err)
		}
	}
	var viewRows int
	if err := db.QueryRow(`SELECT count(*) FROM probe_rule_view`).Scan(&viewRows); err != nil || viewRows != 2 {
		t.Fatalf("view rows=%d err=%v", viewRows, err)
	}
	assertProbeMigrationForeignKeys(t, db)

	for _, statement := range []string{
		`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES('probe-ok','probe',1,'all','probes_down','>',6,0,'warning','created','updated')`,
		`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES('cpu-ok','cpu',1,'all','cpu_percent','>',100,0,'warning','created','updated')`,
		`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES('load-ok','load',1,'all','load1','>',1000000,0,'warning','created','updated')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("valid rule rejected: %v", err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES('cpu-high','cpu',1,'all','cpu_percent','>',101,0,'warning','created','updated')`,
		`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES('offline-bad','offline',1,'all','offline','>',1,180,'warning','created','updated')`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("invalid rule accepted: %s", statement)
		}
	}
	if _, err := db.Exec(`UPDATE alert_rules SET name='renamed' WHERE id='all-rule'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM probe_rule_updates WHERE rule_id='all-rule'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("restored trigger count=%d err=%v", count, err)
	}
	if _, err := db.Exec(`DELETE FROM alert_rules WHERE id='all-rule'; DELETE FROM groups WHERE id='group'; DELETE FROM nodes WHERE id='node'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"alert_rule_channels", "probe_rule_children"} {
		if err := db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE rule_id IN ('all-rule','group-rule','node-rule')`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s cascade left %d rows err=%v", table, count, err)
		}
	}
	if err := db.QueryRow(`SELECT count(*) FROM alert_rules WHERE id IN ('all-rule','group-rule','node-rule')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("generated scope FK cascade left %d rules err=%v", count, err)
	}
	assertProbeMigrationForeignKeys(t, db)
	var migrations int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != migrations {
		t.Fatalf("reopen migration count=%d want=%d err=%v", count, migrations, err)
	}
}

func TestProbeMigrationFailureRollsBackAndRestoresForeignKeys(t *testing.T) {
	forced := errors.New("forced migration failure")
	for _, failure := range []string{"apply_error", "foreign_key_violation"} {
		t.Run(failure, func(t *testing.T) {
			db, _ := openProbeMigrationThirteen(t)
			before := probeMigrationSnapshot(t, db, `SELECT * FROM alert_rules ORDER BY id`)
			err := applyMigration(context.Background(), db, Migration{
				Version: 14, SQL: probesSchema, DisableForeignKeys: true,
				Apply: func(ctx context.Context, tx *sql.Tx) error {
					if err := rebuildProbeAlertRules(ctx, tx); err != nil {
						return err
					}
					if failure == "apply_error" {
						return forced
					}
					_, err := tx.ExecContext(ctx, `INSERT INTO probe_rule_children VALUES('orphan','missing','invalid')`)
					return err
				},
			})
			if failure == "apply_error" && !errors.Is(err, forced) {
				t.Fatalf("migration error=%v, want forced failure", err)
			}
			if failure == "foreign_key_violation" && (err == nil || !strings.Contains(err.Error(), "foreign_key_check")) {
				t.Fatalf("migration error=%v, want foreign_key_check failure", err)
			}
			assertProbeMigrationForeignKeys(t, db)
			if after := probeMigrationSnapshot(t, db, `SELECT * FROM alert_rules ORDER BY id`); !reflect.DeepEqual(before, after) {
				t.Fatalf("rollback changed rules: %v", after)
			}
			for query, want := range map[string]int{
				`SELECT count(*) FROM alert_rule_channels`:                                                              3,
				`SELECT count(*) FROM probe_rule_children`:                                                              3,
				`SELECT count(*) FROM schema_migrations WHERE version=14`:                                               0,
				`SELECT count(*) FROM sqlite_schema WHERE name IN ('probe_latest','probe_hourly','alert_rules_probes')`: 0,
			} {
				var got int
				if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
					t.Fatalf("%s=%d want=%d err=%v", query, got, want, err)
				}
			}
		})
	}
}
