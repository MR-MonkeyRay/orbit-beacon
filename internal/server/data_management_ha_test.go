package server_test

import (
	"context"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLDataManagementHACleanupReplicates(t *testing.T) {
	native := storagetest.Start(t)
	standby := native.Endpoint("standby")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbs, err := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: native.Endpoint("primary"), Standby: &standby}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	f := dataManagementHTTP(t, dbs)
	if err = dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(lease.Context(), "INSERT INTO audit_events(id,action,target_id,created_at,details) VALUES('ha-old','fixture','','2000-01-01T00:00:00.000000000Z','{}')"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	target := native.RawEndpoint(t, "standby", storage.MainDomain)
	await := func(deleted bool) {
		t.Helper()
		for {
			var old, audit int
			err := target.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_events WHERE id='ha-old'").Scan(&old)
			if err == nil {
				err = target.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_events WHERE action='data.cleanup'").Scan(&audit)
			}
			if err == nil && ((!deleted && old == 1) || (deleted && old == 0 && audit > 0)) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("cleanup replication did not converge old=%d audit=%d err=%v", old, audit, err)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	await(false)
	cookie, csrf := login(t, f, "admin")
	response := request(t, f.handler, "POST", "/api/v1/data-management/cleanup", `{"categories":["audit_events"],"keep_days":30,"confirmation":"CLEAN","before":"2026-01-01T00:00:00Z"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 200)
	await(true)
}
