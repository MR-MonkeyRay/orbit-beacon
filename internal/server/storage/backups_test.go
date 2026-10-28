package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupTasksSerializePlanAndScheduledRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insert := func(id, state, scheduled string) error {
		_, err := db.Exec(`INSERT INTO backup_tasks(id,kind,state,phase,plan_id,categories_json,destination_json,scheduled_at,created_at,error_code,warning_code) VALUES(?,'backup_local',?,'','deleted-plan','["configuration"]','{}',?,'2026-10-27T00:00:00.000000000Z','','')`, id, state, scheduled)
		return err
	}
	if err := insert("first", "queued", "2026-10-27T00:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	if err := insert("second", "running", "2026-10-28T00:00:00.000000000Z"); err == nil {
		t.Fatal("two active tasks for same plan accepted")
	}
	if _, err := db.Exec(`UPDATE backup_tasks SET state='succeeded' WHERE id='first'`); err != nil {
		t.Fatal(err)
	}
	if err := insert("duplicate", "queued", "2026-10-27T00:00:00.000000000Z"); err == nil {
		t.Fatal("same scheduled run duplicated after terminal transition")
	}
	if err := insert("next", "queued", "2026-10-28T00:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
}

func TestInboxSourceTransitionAndReadIsolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES('a','a','hash','now'),('b','b','hash','now')`); err != nil {
		t.Fatal(err)
	}
	var indexes int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='inbox_reads_notification'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("notification read index=%d err=%v", indexes, err)
	}
	insert := `INSERT INTO inbox_notifications(id,kind,source_id,transition,severity,created_at,details_json) VALUES(?,'alert','episode','firing','warning','now','{}')`
	if _, err = db.Exec(insert, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(insert, "duplicate"); err == nil {
		t.Fatal("source transition duplicated")
	}
	if _, err = db.Exec(`INSERT INTO inbox_reads VALUES('a','one','now')`); err != nil {
		t.Fatal(err)
	}
	var unread int
	if err = db.QueryRow(`SELECT count(*) FROM inbox_notifications n WHERE NOT EXISTS(SELECT 1 FROM inbox_reads r WHERE r.user_id='b' AND r.notification_id=n.id)`).Scan(&unread); err != nil || unread != 1 {
		t.Fatalf("other user's unread=%d err=%v", unread, err)
	}
	if _, err = db.Exec(`DELETE FROM inbox_notifications WHERE id='one'`); err != nil {
		t.Fatal(err)
	}
	var reads int
	if err = db.QueryRow(`SELECT count(*) FROM inbox_reads`).Scan(&reads); err != nil || reads != 0 {
		t.Fatalf("orphan reads=%d err=%v", reads, err)
	}
}
