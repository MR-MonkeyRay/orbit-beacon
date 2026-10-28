package backup

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func workerFixture(t *testing.T) (*sql.DB, Options) {
	t.Helper()
	dir := restoreFixture(t, "worker")
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`DELETE FROM backup_plans`); err != nil {
		t.Fatal(err)
	}
	key, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	return db, Options{DataDir: dir, SecretBox: box, ServerVersion: "test"}
}

func TestEveryExistingAdministratorCanCreateExport(t *testing.T) {
	db, opts := workerFixture(t)
	second := "20000000-0000-4000-8000-000000000002"
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES(?,'admin-two',?,?)`, second, validationHash, validationTime); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []struct{ id, name string }{{restoreUser, "worker"}, {second, "admin-two"}} {
		task, err := s.CreateExport(context.Background(), actor.id, []Category{Configuration})
		if err != nil || task.ActorID == nil || *task.ActorID != actor.id || task.Destination.ActorName != actor.name {
			t.Fatalf("administrator %s export=%+v err=%v", actor.id, task, err)
		}
	}
	_, err = s.CreateExport(context.Background(), "20000000-0000-4000-8000-000000000003", []Category{Configuration})
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != "invalid_actor" {
		t.Fatalf("nonexistent actor: %v", err)
	}
	var tasks, audits int
	if err = db.QueryRow(`SELECT count(*) FROM backup_tasks WHERE kind='backup_export'`).Scan(&tasks); err != nil || tasks != 2 {
		t.Fatalf("export tasks=%d err=%v", tasks, err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='backup.export'`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("export audits=%d err=%v", audits, err)
	}
}

func TestExportAcceptancePersistsAfterRequestCancellationAndBoundsQueue(t *testing.T) {
	db, opts := workerFixture(t)
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	task, err := s.CreateExport(ctx, restoreUser, []Category{Events, Configuration})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if task.State != "queued" || len(task.Categories) != 2 || task.Categories[0] != Configuration || task.Destination.ActorName != "worker" {
		t.Fatalf("unsafe/noncanonical acceptance: %+v", task)
	}
	stored, err := s.loadTask(context.Background(), task.ID)
	if err != nil || stored.State != "queued" {
		t.Fatalf("request cancellation lost task: %v", err)
	}
	for i := 1; i < 32; i++ {
		if _, err = s.CreateExport(context.Background(), restoreUser, []Category{Configuration}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.CreateExport(context.Background(), restoreUser, []Category{Configuration})
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != "backup_queue_full" {
		t.Fatalf("queue bound: %v", err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='backup.export'`).Scan(&count); err != nil || count != 32 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
}

func TestServiceRejectsMissingAndWrongSecretBox(t *testing.T) {
	db, opts := workerFixture(t)
	saved := opts.SecretBox
	opts.SecretBox = nil
	if _, err := New(db, opts); err == nil {
		t.Fatal("nil keybox accepted")
	}
	opts.SecretBox = saved
	wrong, err := secretbox.New(make([]byte, 32), "orbitbeacon backup secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	opts.SecretBox = wrong
	if _, err = New(db, opts); err == nil {
		t.Fatal("keybox not matching data-dir accepted")
	}
}

func waitWorkerTask(t *testing.T, db *sql.DB, id, state string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got string
		if err := db.QueryRow(`SELECT state FROM backup_tasks WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not become %s", id, state)
}

func insertWorkerTask(t *testing.T, db *sql.DB, id, kind, state, phase string, destination string, cipher []byte) {
	t.Helper()
	at := timekey.Format(time.Now().Add(-100 * 24 * time.Hour))
	_, err := db.Exec(`INSERT INTO backup_tasks(id,kind,state,phase,plan_id,categories_json,destination_json,created_at,password_ciphertext,input_credentials_ciphertext) VALUES(?,?,?,?,?,'["configuration"]',?,?,?,?)`, id, kind, state, phase, restorePlan, destination, at, cipher, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
}
