package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestProbeMigrationCreatesTablesConstraintsAndRetentionIndex(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(privateDirectory(t), "probes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO nodes(id,name,created_at) VALUES('node','node','now')`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"probe_latest", "probe_hourly"} {
		var sqlText string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&sqlText); err != nil {
			t.Fatalf("read %s schema: %v", name, err)
		}
		if sqlText == "" {
			t.Fatalf("%s has empty schema", name)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='probe_hourly_retention'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("probe_hourly_retention index count=%d err=%v", count, err)
	}
	if _, err := db.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES('probe-rule','probe rule',1,'all','probes_down','>',6,0,'warning','now','now')`); err != nil {
		t.Fatalf("fresh schema rejected probes_down: %v", err)
	}
	for _, statement := range []string{
		`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES('cpu-high','cpu high',1,'all','cpu_percent','>',101,0,'warning','now','now')`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("fresh alert schema accepted invalid rule: %s", statement)
		}
	}
	for _, statement := range []string{
		`INSERT INTO probe_latest(node_id,target,ok,error,checked_at) VALUES('node','tcp://host:80',2,'','now')`,
		`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,checked_at) VALUES('node','tcp://host:80',1,-1,'','now')`,
		`INSERT INTO probe_latest(node_id,target,ok,error,checked_at) VALUES('node','tcp://host:80',0,'bogus','now')`,
		`INSERT INTO probe_latest(node_id,target,ok,error,status_code,checked_at) VALUES('node','tcp://host:80',0,'http_status',99,'now')`,
		`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES('node','tcp://host:80','now',1,2,0)`,
		`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES('node','tcp://host:80','now',1,0,-1)`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("probe schema accepted invalid row: %s", statement)
		}
	}
	if _, err := db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES('node','https://example.test',1,12.5,'',200,'now')`); err != nil {
		t.Fatalf("probe_latest rejected valid row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES('node','https://example.test','now',1,0,12.5)`); err != nil {
		t.Fatalf("probe_hourly rejected valid row: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM nodes WHERE id='node'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s cascade left %d rows: %v", table, count, err)
		}
	}
}
