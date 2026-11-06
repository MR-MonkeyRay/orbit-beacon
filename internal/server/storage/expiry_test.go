package storage

import (
	"database/sql"
	"testing"
)

func TestExpirySchemaRealSQLiteRuleCreation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(mainSchema); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		metric, comparator string
		threshold          any
		seconds            int
		valid              bool
	}{
		{"node_expiry_days", "<", 30, 0, true}, {"node_expiry_days", "<", 365, 0, true},
		{"node_expiry_days", "<", 0, 0, false}, {"node_expiry_days", "<", 366, 0, false}, {"node_expiry_days", ">", 30, 0, false},
		{"traffic_quota_percent", ">", 1000, 0, true}, {"traffic_quota_percent", "<", 1000, 0, true},
		{"load1", ">", 1000000, 0, true}, {"cpu_percent", ">", 100, 0, true},
		{"memory_used_percent", ">", 80, 0, true}, {"swap_used_percent", ">", 80, 0, true},
		{"disk_used_percent", ">", 80, 0, true}, {"probes_down", ">", 1, 0, true}, {"offline", "", nil, 180, true},
	} {
		t.Run(tc.metric, func(t *testing.T) {
			var comparator any = tc.comparator
			if tc.metric == "offline" {
				comparator = nil
			}
			_, err := db.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES(lower(hex(randomblob(16))),'Rule',1,'all',?,?,?,?, 'warning','now','now')`, tc.metric, comparator, tc.threshold, tc.seconds)
			if (err == nil) != tc.valid {
				t.Fatalf("%+v: %v", tc, err)
			}
		})
	}
	if _, err = db.Exec("INSERT INTO nodes(id,name,created_at,expires_on) VALUES('expiry','Node','now','2028-02-29'),('unset','Node','now',NULL)"); err != nil {
		t.Fatal(err)
	}
	var date string
	if err = db.QueryRow("SELECT expires_on FROM nodes WHERE id='expiry'").Scan(&date); err != nil || date != "2028-02-29" {
		t.Fatalf("date=%s err=%v", date, err)
	}
}
