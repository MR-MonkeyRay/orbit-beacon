package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestBaselineTwoSnapshotReplace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := OpenMain(ctx, filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	lease, err := source.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if tx.Dialect() != SQLite {
		t.Fatal("wrong dialect")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?)", "empty", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO node_deletions VALUES(?,?)", "deleted", "2026-11-07T00:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE inbox_sequence SET value=41 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO groups(id,name,display_order) VALUES('group','group',321)"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO service_groups(id,name,display_order) VALUES('sg','monitor group',42)"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO services(id,name,group_id,type,target,enabled,public,notify,created_at,updated_at) VALUES('s','monitor','sg','tcp','localhost:443',1,0,0,'now','now')"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "image.db")
	if err = lease.Snapshot(ctx, image); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	target, err := OpenMain(ctx, filepath.Join(dir, "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err = ReplaceDomain(ctx, target, image, "replace-test"); err != nil {
		t.Fatal(err)
	}
	writer, err := target.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	changed, err := writer.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Rollback()
	if _, err = changed.ExecContext(ctx, "UPDATE settings SET value='after-first' WHERE key='empty'"); err != nil {
		t.Fatal(err)
	}
	if err = changed.Commit(); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if err = ReplaceDomain(ctx, target, image, "replace-test"); err != nil {
		t.Fatal(err)
	}
	l, err := target.Acquire(ctx, Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var value int
	if err = l.QueryRowContext(ctx, "SELECT value FROM inbox_sequence WHERE id=1").Scan(&value); err != nil || value != 41 {
		t.Fatalf("counter=%d err=%v", value, err)
	}
	var groupOrder int
	if err = l.QueryRowContext(ctx, "SELECT display_order FROM groups WHERE id='group'").Scan(&groupOrder); err != nil || groupOrder != 321 {
		t.Fatalf("group order=%d err=%v", groupOrder, err)
	}
	var serviceGroup string
	if err = l.QueryRowContext(ctx, "SELECT group_id FROM services WHERE id='s'").Scan(&serviceGroup); err != nil || serviceGroup != "sg" {
		t.Fatalf("service group=%s err=%v", serviceGroup, err)
	}
	if err = l.QueryRowContext(ctx, "SELECT display_order FROM service_groups WHERE id='sg'").Scan(&groupOrder); err != nil || groupOrder != 42 {
		t.Fatalf("service group order=%d err=%v", groupOrder, err)
	}
	var count int
	if err = l.QueryRowContext(ctx, "SELECT count(*) FROM node_deletions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("tombstones=%d err=%v", count, err)
	}
	var setting string
	if err = l.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='empty'").Scan(&setting); err != nil || setting != "after-first" {
		t.Fatalf("replayed operation replaced newer data: %q %v", setting, err)
	}
}

func TestRegistryMatchesSQLiteBaseline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		var db *sql.DB
		var err error
		if domain == MainDomain {
			db, err = Open(ctx, filepath.Join(dir, "main.db"))
		} else {
			db, err = OpenHistorySQLite(ctx, filepath.Join(dir, "history.db"))
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range Tables(domain) {
			rows, err := db.QueryContext(ctx, "PRAGMA table_xinfo(`"+table.Name+"`)")
			if err != nil {
				t.Fatal(err)
			}
			actual := map[string]bool{}
			for rows.Next() {
				var cid, notnull, pk, hidden int
				var name, typ string
				var def any
				if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk, &hidden); err != nil {
					t.Fatal(err)
				}
				actual[name] = hidden != 0
			}
			if err = rows.Close(); err != nil {
				t.Fatal(err)
			}
			if len(actual) != len(table.Columns) {
				t.Fatalf("%s registry columns=%d schema=%d", table.Name, len(table.Columns), len(actual))
			}
			for _, column := range table.Columns {
				hidden, ok := actual[column.Name]
				if !ok || hidden != column.Generated {
					t.Fatalf("%s.%s missing/generated mismatch", table.Name, column.Name)
				}
			}
		}
		db.Close()
	}
}

func TestMySQLNumericErrors(t *testing.T) {
	if !IsBusy(&mysql.MySQLError{Number: 1213, Message: "secret"}) {
		t.Fatal("deadlock not busy")
	}
	if !IsUnique(&mysql.MySQLError{Number: 1062, Message: "secret"}) {
		t.Fatal("duplicate not unique")
	}
	if IsUnique(errors.New("1062 duplicate secret")) {
		t.Fatal("matched text")
	}
}

func TestBaselinePrimaryKeysCannotContainNull(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	db, err := Open(ctx, filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,created_at) VALUES(NULL,'null-key','hash','now')"); err == nil {
		t.Fatal("NULL application primary key accepted")
	}
}
