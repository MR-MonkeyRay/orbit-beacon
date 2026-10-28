package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTrafficMigrationFromTwelvePreservesNodesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "v12.db")
	ctx := context.Background()
	throughTwelve := []Migration{
		{Version: 1, SQL: foundationSchema},
		{Version: 2, SQL: sessionsExpirySchema},
		{Version: 3, SQL: reportIdentitySchema},
		{Version: 4, SQL: enrollmentTombstoneSchema},
		{Version: 5, Apply: repairReportSelections},
		{Version: 6, SQL: recoverableCSRFSchema},
		{Version: 7, SQL: auditOrderingSchema},
		{Version: 8, SQL: enrollmentOrderingSchema},
		{Version: 9, Apply: normalizeTimestampColumns},
		{Version: 10, SQL: alertsSchema},
		{Version: 11, SQL: notificationsSchema},
		{Version: 12},
	}
	db, err := OpenWithMigrations(ctx, path, throughTwelve)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nodes(id,name,created_at) VALUES('retained','retained','2026-10-27T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var nodeCount, migrationCount int
	if err := db.QueryRow(`SELECT count(*) FROM nodes WHERE id='retained'`).Scan(&nodeCount); err != nil {
		t.Fatal(err)
	}
	if nodeCount != 1 {
		t.Fatalf("retained node count=%d", nodeCount)
	}
	if _, err := db.Exec(`INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES('retained','epoch','2026-10-27T00:00:00.000000000Z',1,2,'2026-10-27T00:01:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES('retained','2026-10-26',3,4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM nodes WHERE id='retained'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"traffic_hourly", "traffic_daily"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s cascade left %d rows", table, count)
		}
	}
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reopenedCount int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&reopenedCount); err != nil {
		t.Fatal(err)
	}
	if reopenedCount != migrationCount {
		t.Fatalf("reopen reapplied migration: before=%d after=%d", migrationCount, reopenedCount)
	}
}
