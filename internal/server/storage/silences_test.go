package storage

import (
	"database/sql"
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
	if _, err = db.Exec(`INSERT INTO groups(id,name) VALUES('g','group'); INSERT INTO nodes(id,name,created_at) VALUES('n','node','now');`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind       string
		id         any
		start, end string
		valid      bool
	}{
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
	if _, err = db.Exec(`INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES('all','reason','all',NULL,'a','b','a'),('node','reason','node','n','a','b','a'),('group','reason','group','g','a','b','a'); DELETE FROM groups; DELETE FROM nodes;`); err != nil {
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
