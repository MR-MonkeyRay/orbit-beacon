package storage

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestSilenceSchemaConstraintsAndCascadeRealSQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON;" + mainSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO service_groups(id,name) VALUES('sg','monitors'); INSERT INTO groups(id,name) VALUES('g','group'); INSERT INTO nodes(id,name,created_at) VALUES('n','node','now');`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind       string
		id         any
		start, end string
		valid      bool
	}{
		{"service_group", "sg", "2026-01-01", "2026-01-02", true}, {"service_group", nil, "2026-01-01", "2026-01-02", false}, {"service_group", "missing", "2026-01-01", "2026-01-02", false},
		{"all", nil, "2026-01-01", "2026-01-02", true}, {"node", "n", "2026-01-01", "2026-01-02", true}, {"group", "g", "2026-01-01", "2026-01-02", true},
		{"all", "n", "2026-01-01", "2026-01-02", false}, {"node", nil, "2026-01-01", "2026-01-02", false}, {"group", nil, "2026-01-01", "2026-01-02", false}, {"bad", nil, "2026-01-01", "2026-01-02", false},
		{"node", "missing", "2026-01-01", "2026-01-02", false}, {"group", "missing", "2026-01-01", "2026-01-02", false},
		{"all", nil, "2026-01-02", "2026-01-01", false}, {"all", nil, "2026-01-02", "2026-01-02", false},
	} {
		_, err = db.Exec("INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES('test','reason',?,?,?,?,'now')", tc.kind, tc.id, tc.start, tc.end)
		if (err == nil) != tc.valid {
			t.Fatalf("scope=%s id=%v interval=%s..%s err=%v", tc.kind, tc.id, tc.start, tc.end, err)
		}
		if _, err = db.Exec("DELETE FROM alert_silences"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES('service-group','reason','service_group','sg','a','b','a'),('all','reason','all',NULL,'a','b','a'),('node','reason','node','n','a','b','a'),('group','reason','group','g','a','b','a'); DELETE FROM groups; DELETE FROM nodes; DELETE FROM service_groups;`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM alert_silences WHERE id='all'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("all=%d %v", count, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM alert_silences").Scan(&count); err != nil || count != 1 {
		t.Fatalf("cascade=%d %v", count, err)
	}
	if _, err = db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,started_at,rule_name,node_name,rule_snapshot) VALUES('event','r','n','firing','a','rule','node','{}')`); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT silenced FROM alert_events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("default flag=%d %v", count, err)
	}
	for _, flag := range []any{0, 1, -1, 2, nil} {
		_, err = db.Exec("UPDATE alert_events SET silenced=?", flag)
		want := flag == 0 || flag == 1
		if (err == nil) != want {
			t.Fatalf("silenced=%v err=%v", flag, err)
		}
	}
}

func TestServiceGroupSilenceReplicaRoundTrip(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	for i, query := range []string{
		"INSERT INTO service_groups(id,name) VALUES('sg','monitors')",
		"INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES('s','maintenance','service_group','sg','a','b','a')",
		"DELETE FROM service_groups WHERE id='sg'",
	} {
		replicaWrite(t, source, query)
		batch, err := readReplicaTransaction(t.Context(), source, int64(i))
		if err != nil {
			t.Fatal(err)
		}
		if err = applyReplicaTransaction(t.Context(), target, batch); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = target.current.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM alert_silences").Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if i == 1 {
			want = 1
		}
		if count != want {
			t.Fatalf("step %d count=%d want=%d", i, count, want)
		}
	}
}

func TestServiceGroupSilenceRegistryAndCodec(t *testing.T) {
	var silence TableSpec
	seen := map[string]bool{}
	for _, table := range Tables(MainDomain) {
		for _, fk := range table.FK {
			if !seen[fk.Table] {
				t.Fatalf("%s precedes FK parent %s", table.Name, fk.Table)
			}
		}
		seen[table.Name] = true
		if table.Name == "alert_silences" {
			silence = table
		}
	}
	if silence.ID != 14 {
		t.Fatal(silence)
	}
	wantFK := ForeignKeySpec{"scope_service_group_id", "service_groups", "id", "CASCADE"}
	found := false
	for _, fk := range silence.FK {
		found = found || fk == wantFK
	}
	if !found {
		t.Fatal(silence)
	}
	found = false
	for _, c := range silence.Columns {
		if c.Name == "scope_service_group_id" {
			found = c.Generated && c.Type == Text
		}
	}
	if !found {
		t.Fatal(silence)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(mainSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO service_groups(id,name) VALUES('g','monitors'); INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES('s','maintenance','service_group','g','a','b','a')"); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = db.QueryRow("SELECT " + jsonImage(SQLite, silence, "s", false) + " FROM alert_silences s").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	values, err := decodeValues([]byte(raw), columns(silence))
	if err != nil || !reflect.DeepEqual(values, []any{"s", "maintenance", "service_group", "g", "a", "b", "a"}) {
		t.Fatalf("values=%v err=%v", values, err)
	}
	for _, ddl := range mysqlMainSchema {
		if strings.HasPrefix(ddl, "CREATE TABLE `alert_silences`") {
			if !strings.Contains(ddl, "`scope_service_group_id` VARBINARY(64) GENERATED ALWAYS AS") || !strings.Contains(ddl, "REFERENCES `service_groups`(`id`) ON DELETE CASCADE") || !strings.Contains(ddl, "CHECK((scope_kind='all' AND scope_id IS NULL) OR (scope_kind IN ('group','node','service_group') AND scope_id IS NOT NULL))") {
				t.Fatal(ddl)
			}
		}
	}
}
