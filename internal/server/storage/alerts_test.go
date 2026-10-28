package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAlertSchemaScopeConstraintsAndCascades(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alerts.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	for _, table := range []string{"alert_rules", "alert_events"} {
		if err = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if _, err = db.Exec(`
INSERT INTO groups(id,name) VALUES('group','group');
INSERT INTO nodes(id,name,created_at) VALUES('node','node','now');
INSERT INTO alert_rules(id,name,enabled,scope_kind,scope_id,metric,for_seconds,severity,created_at,updated_at) VALUES
 ('group-rule','group offline',1,'group','group','offline',180,'critical','now','now'),
 ('node-rule','node offline',1,'node','node','offline',180,'critical','now','now'),
 ('all-rule','all offline',1,'all',NULL,'offline',180,'critical','now','now');`); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []struct {
		id    string
		group string
		node  string
	}{
		{"group-rule", "group", ""},
		{"node-rule", "", "node"},
		{"all-rule", "", ""},
	} {
		var group, node string
		if err := db.QueryRow(`SELECT coalesce(scope_group_id,''),coalesce(scope_node_id,'') FROM alert_rules WHERE id=?`, rule.id).Scan(&group, &node); err != nil {
			t.Fatal(err)
		}
		if group != rule.group || node != rule.node {
			t.Fatalf("%s generated scope group=%q node=%q", rule.id, group, node)
		}
	}
	for _, scope := range []struct {
		kind string
		id   any
	}{
		{"group", "missing"},
		{"node", "missing"},
		{"all", "node"},
		{"group", nil},
		{"node", nil},
	} {
		if _, err := db.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,scope_id,metric,for_seconds,severity,created_at,updated_at) VALUES('invalid','invalid',1,?,?,'offline',180,'warning','now','now')`, scope.kind, scope.id); err == nil {
			t.Fatalf("accepted invalid scope kind=%s id=%v", scope.kind, scope.id)
		}
	}
	if _, err = db.Exec(`DELETE FROM groups WHERE id='group'; DELETE FROM nodes WHERE id='node'`); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM alert_rules WHERE id IN ('group-rule','node-rule')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("generated scope FK cascade left %d rules: %v", count, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM alert_rules WHERE id='all-rule'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("inventory deletion changed all-node rule: count=%d err=%v", count, err)
	}
}
