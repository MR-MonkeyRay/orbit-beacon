package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestTrafficMigrationCreatesTablesConstraintsAndRetentionIndex(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(privateDirectory(t), "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, name := range []string{"traffic_hourly", "traffic_daily"} {
		var sqlText string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&sqlText); err != nil {
			t.Fatalf("read %s schema: %v", name, err)
		}
		if sqlText == "" {
			t.Fatalf("%s has empty schema", name)
		}
	}
	var count int
	for _, name := range []string{"traffic_hourly_retention", "traffic_daily_retention"} {
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s index count=%d", name, count)
		}
	}
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 14 || storage.CurrentSchemaVersion != 14 {
		t.Fatalf("schema version database=%d binary=%d", count, storage.CurrentSchemaVersion)
	}
	if _, err := db.Exec(`INSERT INTO nodes(id,name,created_at) VALUES('node','node','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES('node','epoch','2026-10-27T00:00:00.000000000Z',-1,0,'now')`); err == nil {
		t.Fatal("traffic_hourly accepted a negative counter")
	}
	if _, err := db.Exec(`INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES('node','epoch','2026-10-27T00:00:00.000000000Z',0,-1,'now')`); err == nil {
		t.Fatal("traffic_hourly accepted a negative transmitted counter")
	}
}
