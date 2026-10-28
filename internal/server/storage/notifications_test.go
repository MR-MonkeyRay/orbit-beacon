package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNotificationSchemaRoutesAndChannelCascades(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "notifications.db")
	ctx := context.Background()
	db, e := Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	_, e = db.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,for_seconds,severity,created_at,updated_at) VALUES('rule','retained',1,'all','offline',180,'warning','now','now')`)
	if e != nil {
		t.Fatal(e)
	}
	var count int
	for _, table := range []string{"notification_channels", "notification_deliveries", "alert_rule_channels", "notification_rate_limits"} {
		if e = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); e != nil || count != 0 {
			t.Fatal(table, e)
		}
	}
	if _, e = db.Exec(`INSERT INTO notification_channels(id,name,kind,enabled,config_json,secret_ciphertext,created_at,updated_at) VALUES('channel','webhook','webhook',1,'{}',NULL,'now','now'); INSERT INTO alert_rule_channels VALUES('rule','channel'); DELETE FROM alert_rules WHERE id='rule'`); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow("SELECT count(*) FROM alert_rule_channels").Scan(&count); e != nil || count != 0 {
		t.Fatalf("rule FK cascade left %d routes: %v", count, e)
	}
	if _, e = db.Exec(`
INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,for_seconds,severity,created_at,updated_at) VALUES('rule-two','offline',1,'all','offline',180,'warning','now','now');
INSERT INTO alert_rule_channels(rule_id,channel_id) VALUES('rule-two','channel');
INSERT INTO notification_rate_limits(channel_id,window_start,count) VALUES('channel','now',1);
INSERT INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,payload_json,created_at,updated_at) VALUES('delivery','channel','event','pending','now','{}','now','now');
DELETE FROM notification_channels WHERE id='channel';`); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"alert_rule_channels", "notification_rate_limits", "notification_deliveries"} {
		if e = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); e != nil || count != 0 {
			t.Fatalf("channel FK cascade left %d %s rows: %v", count, table, e)
		}
	}
	if e = db.QueryRow("SELECT count(*) FROM alert_rules WHERE id='rule-two'").Scan(&count); e != nil || count != 1 {
		t.Fatalf("channel deletion changed rule: count=%d err=%v", count, e)
	}
	for _, index := range []string{"notification_deliveries_due", "notification_deliveries_retention", "notification_deliveries_history"} {
		if e = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&count); e != nil || count != 1 {
			t.Fatalf("missing index %s: count=%d err=%v", index, count, e)
		}
	}
}
