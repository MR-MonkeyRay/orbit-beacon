package server_test

import (
	"encoding/json"
	"testing"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

func dataManagementTerminalAndExcludedRows(t *testing.T, f fixture, dbs *storage.Databases) {
	t.Helper()
	node, err := f.nodes.CreatePending(t.Context(), nodes.CreateRequest{Name: "preserved"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := dbs.Main.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var userID string
	if err = tx.QueryRowContext(lease.Context(), "SELECT id FROM users LIMIT 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO settings(`key`,value) VALUES('preserved','yes')", nil},
		{"INSERT INTO traffic_daily VALUES(?,'2000-01-01',1,2)", []any{node.ID}},
		{"INSERT INTO probe_latest(node_id,target,ok,error,checked_at) VALUES(?,'latest',1,'','2000-01-01T00:00:00.000000000Z')", []any{node.ID}},
		{"INSERT INTO backup_tasks(id,kind,state,categories_json,destination_json,created_at) VALUES('backup','backup_export','queued','[]','{}','2000-01-01T00:00:00.000000000Z')", nil},
		{"INSERT INTO notification_channels(id,name,kind,enabled,config_json,created_at,updated_at) VALUES('channel','c','webhook',1,'{}','2000-01-01T00:00:00.000000000Z','2000-01-01T00:00:00.000000000Z')", nil},
		{"INSERT INTO node_availability_daily VALUES(?,'2000-01-01',1,0)", []any{node.ID}},
		{"INSERT INTO node_availability_daily VALUES(?,'2026-01-01',1,0)", []any{node.ID}},
		{"INSERT INTO probe_hourly VALUES(?,'target','2000-01-01T00:00:00.000000000Z',1,0,1,1)", []any{node.ID}},
		{"INSERT INTO probe_hourly VALUES(?,'target','2999-01-01T00:00:00.000000000Z',1,0,1,1)", []any{node.ID}},
		{"INSERT INTO alert_events(id,rule_id,node_id,state,started_at,resolved_at,rule_name,node_name,rule_snapshot) VALUES('resolved','r',?,'resolved','2000-01-01T00:00:00.000000000Z','2000-01-01T00:00:00.000000000Z','r','n','{}')", []any{node.ID}},
		{"INSERT INTO alert_events(id,rule_id,node_id,state,started_at,rule_name,node_name,rule_snapshot) VALUES('firing','r',?,'firing','2000-01-01T00:00:00.000000000Z','r','n','{}')", []any{node.ID}},
		{"INSERT INTO inbox_notifications(sequence,id,kind,source_id,transition,severity,created_at,details_json) VALUES(1,'old-notice','task','backup','failed','warning','2000-01-01T00:00:00.000000000Z','{}')", nil},
		{"INSERT INTO inbox_notifications(sequence,id,kind,source_id,transition,severity,created_at,details_json) VALUES(2,'live-notice','task','backup','succeeded','info','2999-01-01T00:00:00.000000000Z','{}')", nil},
		{"UPDATE inbox_sequence SET value=2 WHERE id=1", nil},
		{"INSERT INTO inbox_reads VALUES(?,'old-notice','2000-01-01T00:00:00.000000000Z')", []any{userID}},
		{"INSERT INTO inbox_reads VALUES(?,'live-notice','2999-01-01T00:00:00.000000000Z')", []any{userID}},
	}
	for _, q := range queries {
		if _, err = tx.ExecContext(lease.Context(), q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{"sent", "failed", "pending"} {
		if _, err = tx.ExecContext(lease.Context(), "INSERT INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,payload_json,created_at,updated_at) VALUES(?,'channel','firing',?,'2000-01-01T00:00:00.000000000Z','{}','2000-01-01T00:00:00.000000000Z','2000-01-01T00:00:00.000000000Z')", state, state); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := login(t, f, "admin")
	statsResponse := request(t, f.handler, "GET", "/api/v1/data-management?keep_days=30", "", cookie, "", "")
	assertHTTPStatus(t, statsResponse, 200)
	var stats struct {
		Categories []struct {
			ID       string `json:"id"`
			Rows     int64  `json:"rows"`
			Eligible int64  `json:"eligible_rows"`
		} `json:"categories"`
	}
	if err = json.Unmarshal(statsResponse.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	expected := map[string][2]int64{"probe_history": {2, 1}, "availability_history": {2, 2}, "resolved_alerts": {2, 1}, "notification_history": {2, 1}, "delivery_history": {3, 2}}
	for _, category := range stats.Categories {
		if want, ok := expected[category.ID]; ok {
			if category.Rows != want[0] || category.Eligible != want[1] {
				t.Fatalf("stats predicate mismatch: %+v want=%v", category, want)
			}
			delete(expected, category.ID)
		}
	}
	if len(expected) > 0 {
		t.Fatalf("missing categories: %v", expected)
	}
	response := request(t, f.handler, "POST", "/api/v1/data-management/cleanup", `{"categories":["probe_history","availability_history","resolved_alerts","notification_history","delivery_history"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 200)
	var result struct {
		Results []struct {
			ID      string `json:"id"`
			Deleted int64  `json:"deleted_rows"`
			More    bool   `json:"has_more"`
		} `json:"results"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 5 {
		t.Fatalf("missing cleanup results: %s", response.Body)
	}
	for _, category := range result.Results {
		want := int64(1)
		if category.ID == "delivery_history" {
			want = 2
		}
		if category.Deleted != want || category.More {
			t.Fatalf("incorrect cleanup count: %+v", category)
		}
	}
	for _, check := range []struct {
		query string
		want  int
	}{
		{"SELECT COUNT(*) FROM probe_hourly", 1},
		{"SELECT COUNT(*) FROM node_availability_daily WHERE day='2026-01-01'", 1},
		{"SELECT COUNT(*) FROM node_availability_daily WHERE day='2000-01-01'", 0},
		{"SELECT COUNT(*) FROM alert_events WHERE state='firing'", 1},
		{"SELECT COUNT(*) FROM alert_events WHERE state='resolved'", 0},
		{"SELECT COUNT(*) FROM notification_deliveries WHERE state='pending'", 1},
		{"SELECT COUNT(*) FROM notification_deliveries WHERE state IN ('sent','failed')", 0},
		{"SELECT COUNT(*) FROM inbox_notifications", 1},
		{"SELECT COUNT(*) FROM inbox_reads WHERE notification_id='old-notice'", 0},
		{"SELECT COUNT(*) FROM inbox_reads WHERE notification_id='live-notice'", 1},
		{"SELECT value FROM inbox_sequence WHERE id=1", 2},
		{"SELECT COUNT(*) FROM backup_tasks", 1},
		{"SELECT COUNT(*) FROM nodes", 1},
		{"SELECT COUNT(*) FROM traffic_daily", 1},
		{"SELECT COUNT(*) FROM probe_latest", 1},
		{"SELECT COUNT(*) FROM settings WHERE `key`='preserved'", 1},
	} {
		var count int
		if err = lease.QueryRowContext(lease.Context(), check.query).Scan(&count); err != nil || count != check.want {
			t.Fatalf("%s = %d, want %d: %v", check.query, count, check.want, err)
		}
	}
}
