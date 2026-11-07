package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func replicaNativeObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT table_name,'table',COALESCE(engine,'') FROM information_schema.tables WHERE table_schema=DATABASE() UNION ALL SELECT trigger_name,'trigger',action_statement FROM information_schema.triggers WHERE trigger_schema=DATABASE() ORDER BY 1,2")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, kind, definition string
		if err = rows.Scan(&name, &kind, &definition); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join([]string{name, kind, definition}, "|"))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestMySQLReplicaPartialProtocolDoesNotRepairOrChangeData(t *testing.T) {
	ctx := context.Background()
	f := storagetest.Start(t)
	standby := f.Endpoint("standby")
	cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('partial-protocol','preserved')"); err != nil {
			t.Fatal(err)
		}
	})
	if err = dbs.Close(); err != nil {
		t.Fatal(err)
	}
	raw := f.RawEndpoint(t, "primary", storage.MainDomain)
	if _, err = raw.ExecContext(ctx, "DROP TABLE replica_changes"); err != nil {
		t.Fatal(err)
	}
	before := replicaNativeObjects(t, raw)
	dbs, err = storage.OpenDatabases(ctx, cfg, dir)
	if dbs != nil {
		dbs.Close()
	}
	if !errors.Is(err, storage.ErrIdentityMismatch) {
		t.Fatal("partial registered protocol accepted", err)
	}
	after := replicaNativeObjects(t, raw)
	if !reflect.DeepEqual(before, after) {
		t.Error("rejected partial protocol changed native objects")
	}
	var value string
	if err = raw.QueryRow("SELECT value FROM settings WHERE `key`='partial-protocol'").Scan(&value); err != nil || value != "preserved" {
		t.Fatal("rejected partial protocol changed application data", err)
	}
}
