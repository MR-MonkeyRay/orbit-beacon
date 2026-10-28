package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNotificationMigrationFromTen(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	path := filepath.Join(dir, "v10.db")
	ctx := context.Background()
	migrations := []Migration{{Version: 1, SQL: foundationSchema}, {Version: 2, SQL: sessionsExpirySchema}, {Version: 3, SQL: reportIdentitySchema}, {Version: 4, SQL: enrollmentTombstoneSchema}, {Version: 5, Apply: repairReportSelections}, {Version: 6, SQL: recoverableCSRFSchema}, {Version: 7, SQL: auditOrderingSchema}, {Version: 8, SQL: enrollmentOrderingSchema}, {Version: 9, Apply: normalizeTimestampColumns}, {Version: 10, SQL: alertsSchema}}
	db, e := OpenWithMigrations(ctx, path, migrations)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,for_seconds,severity,created_at,updated_at) VALUES('rule','retained',1,'all','offline',180,'warning','now','now')`)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	db, e = Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var version, count int
	db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version)
	if version != CurrentSchemaVersion {
		t.Fatalf("version %d", version)
	}
	db.QueryRow("SELECT count(*) FROM alert_rules WHERE id='rule'").Scan(&count)
	if count != 1 {
		t.Fatal("old rule lost")
	}
	for _, table := range []string{"notification_channels", "notification_deliveries", "alert_rule_channels", "notification_rate_limits"} {
		if e = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); e != nil || count != 0 {
			t.Fatal(table, e)
		}
	}
	if _, e = db.Exec(`INSERT INTO notification_channels(id,name,kind,enabled,config_json,secret_ciphertext,created_at,updated_at) VALUES('channel','webhook','webhook',1,'{}',NULL,'now','now'); INSERT INTO alert_rule_channels VALUES('rule','channel'); DELETE FROM alert_rules WHERE id='rule'`); e != nil {
		t.Fatal(e)
	}
	db.QueryRow("SELECT count(*) FROM alert_rule_channels").Scan(&count)
	if count != 0 {
		t.Fatal("rule FK did not cascade")
	}
	for _, index := range []string{"notification_deliveries_due", "notification_deliveries_retention", "notification_deliveries_history"} {
		db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&count)
		if count != 1 {
			t.Fatal("missing index", index)
		}
	}
}
