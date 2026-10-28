package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

const lifecycleID = "10000000-0000-4000-8000-000000000001"

func lifecycleService(t *testing.T) (*Service, time.Time) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon backup secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &Service{db: db, box: box, opts: Options{DataDir: dir, Now: func() time.Time { return now }}}
	t.Cleanup(s.closeCleanup)
	return s, now
}

func lifecycleTask(t *testing.T, s *Service, id, state string, created, finished time.Time) string {
	t.Helper()
	return lifecycleTaskUsing(t, s, s.db, id, state, created, finished)
}

func lifecycleTaskUsing(t *testing.T, s *Service, exec interface {
	Exec(string, ...any) (sql.Result, error)
}, id, state string, created, finished time.Time) string {
	t.Helper()
	name := "orbitbeacon-" + finished.UTC().Format("20060102T150405Z") + "-" + id + ".tar.gz.age"
	password, err := s.box.Seal("task-password:"+id, []byte("aB0123456789XyZq"))
	if err != nil {
		t.Fatal(err)
	}
	var ended, expiry any
	if !finished.IsZero() {
		ended = timekey.Format(finished)
		expiry = timekey.Format(finished.Add(90 * 24 * time.Hour))
	}
	_, err = exec.Exec(`INSERT INTO backup_tasks(id,kind,state,phase,categories_json,destination_json,created_at,finished_at,artifact_name,artifact_bytes,artifact_sha256,artifact_expires_at,artifact_cleanup_after,result_location,password_ciphertext,password_expires_at) VALUES (?,'backup_export',?,'storing','["configuration"]','{}',?,?,?,7,?, ?,?,?,?,?)`, id, state, timekey.Format(created), ended, name, strings.Repeat("a", 64), timekey.Format(finished.Add(24*time.Hour)), timekey.Format(finished.Add(24*time.Hour)), filepath.Join(s.opts.DataDir, "exports", id, name), password, expiry)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func lifecycleFile(t *testing.T, s *Service, id, name string) string {
	t.Helper()
	dir := filepath.Join(s.opts.DataDir, "exports", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func lifecycleCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("error=%v want %s", err, want)
	}
}

func TestTaskLifecyclePasswordVisibilityAndAudit(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	name := lifecycleTask(t, s, lifecycleID, "succeeded", now.Add(-100*24*time.Hour), now.Add(-time.Hour))
	lifecycleFile(t, s, lifecycleID, name)
	task, err := s.Task(ctx, lifecycleID)
	if err != nil {
		t.Fatal(err)
	}
	if !task.PasswordAvailable || !task.DownloadAvailable || task.ArtifactBytes == nil || *task.ArtifactBytes != "7" {
		t.Fatalf("projection=%+v", task)
	}
	encoded, _ := json.Marshal(task)
	if bytes.Contains(encoded, []byte("ciphertext")) || bytes.Contains(encoded, []byte("aB012")) || bytes.Contains(encoded, []byte("backup-work")) {
		t.Fatal("unsafe task JSON")
	}
	actor := "20000000-0000-4000-8000-000000000001"
	if _, err = s.db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES (?,'reader','hash',?)`, actor, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	result, err := s.Password(audit.WithActor(ctx, actor), lifecycleID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Password != "aB0123456789XyZq" || result.ArchiveName != name || result.ArchiveSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("result=%+v", result)
	}
	var details, target string
	if err = s.db.QueryRow(`SELECT details,target_id FROM audit_events WHERE action='backup.password'`).Scan(&details, &target); err != nil {
		t.Fatal(err)
	}
	if details != "{}" || target != lifecycleID {
		t.Fatalf("audit leaked data: %s %s", details, target)
	}
	if _, err = s.db.Exec(`UPDATE backup_tasks SET state='failed',warning_code='storage_outcome_unknown' WHERE id=?`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Password(ctx, lifecycleID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE backup_tasks SET warning_code='' WHERE id=?`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Password(ctx, lifecycleID)
	lifecycleCode(t, err, "password_unavailable")
	if _, err = s.db.Exec(`UPDATE backup_tasks SET state='succeeded',password_expires_at=? WHERE id=?`, timekey.Format(now), lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Password(ctx, lifecycleID)
	lifecycleCode(t, err, "password_expired")
	if _, err = s.db.Exec(`UPDATE backup_tasks SET password_expires_at=?,restored=1 WHERE id=?`, timekey.Format(now.Add(time.Hour)), lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Password(ctx, lifecycleID)
	lifecycleCode(t, err, "password_unavailable")
}

func TestTaskLifecyclePasswordWrongKeyAndIncompleteMetadata(t *testing.T) {
	s, now := lifecycleService(t)
	lifecycleTask(t, s, lifecycleID, "succeeded", now, now)
	s.box, _ = secretbox.New(bytes.Repeat([]byte{9}, 32), "orbitbeacon backup secrets v1")
	_, err := s.Password(context.Background(), lifecycleID)
	lifecycleCode(t, err, "secret_unreadable")
	if _, err = s.db.Exec(`UPDATE backup_tasks SET result_location=NULL WHERE id=?`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	task, err := s.Task(context.Background(), lifecycleID)
	if err != nil {
		t.Fatal(err)
	}
	if task.PasswordAvailable {
		t.Fatal("incomplete metadata available")
	}
}

func TestTaskLifecycleOpenArtifactIsSeekableAndEnforcesOwnership(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	name := lifecycleTask(t, s, lifecycleID, "succeeded", now, now)
	path := lifecycleFile(t, s, lifecycleID, name)
	file, task, err := s.OpenArtifact(ctx, lifecycleID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ArtifactName == nil || *task.ArtifactName != name {
		t.Fatal("missing metadata")
	}
	if _, err = file.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err = io.ReadFull(file, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("range=%q err=%v", buf, err)
	}
	file.Close()
	if err = os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.OpenArtifact(ctx, lifecycleID)
	lifecycleCode(t, err, "artifact_missing")
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE backup_tasks SET artifact_expires_at=? WHERE id=?`, timekey.Format(now), lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.OpenArtifact(ctx, lifecycleID)
	lifecycleCode(t, err, "artifact_expired")
	if _, err = s.db.Exec(`UPDATE backup_tasks SET artifact_expires_at=?,state='running' WHERE id=?`, timekey.Format(now.Add(time.Hour)), lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.OpenArtifact(ctx, lifecycleID)
	lifecycleCode(t, err, "artifact_not_ready")
	if _, err = s.db.Exec(`UPDATE backup_tasks SET state='succeeded',restored=1 WHERE id=?`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.OpenArtifact(ctx, lifecycleID)
	lifecycleCode(t, err, "artifact_missing")
	if _, err = s.db.Exec(`UPDATE backup_tasks SET restored=0,artifact_name='../elsewhere' WHERE id=?`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.OpenArtifact(ctx, lifecycleID)
	lifecycleCode(t, err, "artifact_missing")
}

func TestTaskLifecycleListKeysetAndOldLocatorHidden(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("10000000-0000-4000-8000-%012d", i)
		lifecycleTask(t, s, id, "succeeded", now, now)
	}
	lifecycleTask(t, s, "10000000-0000-4000-8000-000000000004", "failed", now.Add(-100*24*time.Hour), now.Add(-91*24*time.Hour))
	page, err := s.Tasks(ctx, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 2 || !page.HasMore || page.NextCursor == nil || page.Tasks[0].ID != "10000000-0000-4000-8000-000000000003" {
		t.Fatalf("page=%+v", page)
	}
	next, err := s.Tasks(ctx, 2, *page.NextCursor)
	if err != nil || len(next.Tasks) != 1 || next.HasMore {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	for _, limit := range []int{0, 101} {
		_, err = s.Tasks(ctx, limit, "")
		lifecycleCode(t, err, "invalid_input")
	}
	_, err = s.Tasks(ctx, 1, "junk")
	lifecycleCode(t, err, "invalid_input")
	_, err = s.Task(ctx, "10000000-0000-4000-8000-000000000004")
	lifecycleCode(t, err, "task_not_found")
}

func TestTaskLifecycleRetentionActivePasswordAndFinishedLifetime(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	lifecycleTask(t, s, lifecycleID, "running", now.Add(-100*24*time.Hour), time.Time{})
	if _, err := s.db.Exec(`UPDATE backup_tasks SET password_expires_at=? WHERE id=?`, timekey.Format(now.Add(-time.Hour)), lifecycleID); err != nil {
		t.Fatal(err)
	}
	recent := "10000000-0000-4000-8000-000000000002"
	lifecycleTask(t, s, recent, "succeeded", now.Add(-100*24*time.Hour), now.Add(-time.Hour))
	old := "10000000-0000-4000-8000-000000000003"
	name := lifecycleTask(t, s, old, "failed", now.Add(-100*24*time.Hour), now.Add(-91*24*time.Hour))
	lifecycleFile(t, s, old, name)
	s.opts.removeArtifact = func(string) error { return os.ErrPermission }
	if _, err := s.Cleanup(ctx, now, 500); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{lifecycleID, recent} {
		var cipher []byte
		if err := s.db.QueryRow(`SELECT password_ciphertext FROM backup_tasks WHERE id=?`, id).Scan(&cipher); err != nil || len(cipher) == 0 {
			t.Fatalf("password removed %s %v", id, err)
		}
	}
	var cipher []byte
	var after string
	if err := s.db.QueryRow(`SELECT password_ciphertext,artifact_cleanup_after FROM backup_tasks WHERE id=?`, old).Scan(&cipher, &after); err != nil {
		t.Fatal(err)
	}
	if len(cipher) != 0 || after != timekey.Format(now.Add(time.Hour)) {
		t.Fatalf("locator not retained safely: %v %s", cipher, after)
	}
	s.opts.removeArtifact = nil
	if _, err := s.Cleanup(ctx, now.Add(time.Hour), 500); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM backup_tasks WHERE id=?`, old).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old row retained count=%d err=%v", count, err)
	}
}

func TestTaskLifecycleCleanupFairnessAndMissingFileConvergence(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	var first string
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i <= 505; i++ {
		id := fmt.Sprintf("10000000-0000-4000-8000-%012d", i)
		name := lifecycleTaskUsing(t, s, tx, id, "succeeded", now.Add(-100*24*time.Hour), now.Add(-91*24*time.Hour))
		path := lifecycleFile(t, s, id, name)
		if i == 1 {
			first = path
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.opts.removeArtifact = func(path string) error {
		if path == first {
			return os.ErrPermission
		}
		return os.Remove(path)
	}
	for range 4 {
		if _, err := s.Cleanup(ctx, now, 500); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM backup_tasks`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("starvation count=%d err=%v", count, err)
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	s.opts.removeArtifact = nil
	if _, err := s.Cleanup(ctx, now.Add(time.Hour), 500); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM backup_tasks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("ENOENT failed count=%d err=%v", count, err)
	}
}

func TestTaskLifecycleOrphansRespectUUIDAndRestoredOwnership(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	name := lifecycleTask(t, s, lifecycleID, "succeeded", now, now)
	owned := lifecycleFile(t, s, lifecycleID, name)
	orphanID := "10000000-0000-4000-8000-000000000002"
	orphanName := "orbitbeacon-20261027T120000Z-" + orphanID + ".tar.gz.age"
	orphan := lifecycleFile(t, s, orphanID, orphanName)
	foreign := lifecycleFile(t, s, orphanID, "do-not-touch.txt")
	external := filepath.Join(t.TempDir(), "external.age")
	if err := os.WriteFile(external, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE backup_tasks SET restored=1,result_location=? WHERE id=?`, external, lifecycleID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CleanupOrphans(ctx, 500); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{owned, orphan} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan remains %s %v", path, err)
		}
	}
	for _, path := range []string{foreign, external} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("foreign path removed", err)
		}
	}
}

func TestTaskLifecycleLocalRetentionOwnFilesOnly(t *testing.T) {
	s, _ := lifecycleService(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	planID := "30000000-0000-4000-8000-000000000001"
	dir := filepath.Join(root, "orbitbeacon-"+planID)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("orbitbeacon-2026102%dT120000Z-10000000-0000-4000-8000-%012d.tar.gz.age", i, i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("archive"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneLocal(context.Background(), root, planID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("oldest archive remains")
	}
	for _, path := range append(paths[1:], foreign) {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PruneLocal(context.Background(), root, "../bad", 2); err == nil {
		t.Fatal("unsafe plan ID accepted")
	}
}

func TestTaskLifecycleExpiredPasswordStillReportsExpiryAfterCleanup(t *testing.T) {
	s, now := lifecycleService(t)
	lifecycleTask(t, s, lifecycleID, "succeeded", now.Add(-100*24*time.Hour), now.Add(-time.Hour))
	if _, err := s.db.Exec(`UPDATE backup_tasks SET password_expires_at=?,password_ciphertext=NULL WHERE id=?`, timekey.Format(now), lifecycleID); err != nil {
		t.Fatal(err)
	}
	_, err := s.Password(context.Background(), lifecycleID)
	lifecycleCode(t, err, "password_expired")
}

func TestTaskLifecycleArtifactSymlinksNeverFollowed(t *testing.T) {
	s, now := lifecycleService(t)
	name := lifecycleTask(t, s, lifecycleID, "succeeded", now, now)
	external := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(external, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.opts.DataDir, "exports", lifecycleID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.OpenArtifact(context.Background(), lifecycleID)
	lifecycleCode(t, err, "artifact_missing")
	if _, err := s.Cleanup(context.Background(), now.Add(25*time.Hour), 500); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(external)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("external changed: %q %v", contents, err)
	}
}

func TestTaskLifecycleOrphanScanKeepsOwnedAndTemporaryScope(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	name := lifecycleTask(t, s, lifecycleID, "running", now, time.Time{})
	owned := lifecycleFile(t, s, lifecycleID, name)
	orphanID := "10000000-0000-4000-8000-000000000002"
	orphanName := "orbitbeacon-20261027T120000Z-" + orphanID + ".tar.gz.age"
	temporary := lifecycleFile(t, s, orphanID, "."+orphanName+".tmp")
	wrongUUID := lifecycleFile(t, s, orphanID, "."+name+".tmp")
	if _, err := s.CleanupOrphans(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatal("own orphan temporary remains", err)
	}
	for _, path := range []string{owned, wrongUUID} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("owned or foreign temporary removed", err)
		}
	}
	s.closeCleanup()
}

func TestTaskLifecycleLocalTempsOnlyInactiveRecordedOwnFiles(t *testing.T) {
	s, now := lifecycleService(t)
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	planID := "30000000-0000-4000-8000-000000000001"
	dir := filepath.Join(root, "orbitbeacon-"+planID)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target, _ := json.Marshal(TaskDestination{Target: json.RawMessage(fmt.Sprintf(`{"path":%q,"retain_count":7}`, root))})
	var temps []string
	for i, state := range []string{"succeeded", "running", "failed"} {
		id := fmt.Sprintf("10000000-0000-4000-8000-%012d", i+1)
		name := lifecycleTask(t, s, id, state, now, now)
		restored := i == 2
		if _, err := s.db.Exec(`UPDATE backup_tasks SET kind='backup_local',plan_id=?,destination_json=?,restored=? WHERE id=?`, planID, string(target), restored, id); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "."+name+".tmp")
		if err := os.WriteFile(path, []byte("temporary"), 0600); err != nil {
			t.Fatal(err)
		}
		temps = append(temps, path)
	}
	if _, err := s.CleanupLocalTemps(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temps[0]); !os.IsNotExist(err) {
		t.Fatal("inactive temporary remains", err)
	}
	for _, path := range temps[1:] {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("active or restored temporary removed", err)
		}
	}
}
func TestTaskLifecycleInactiveExportTemporaryRemovedWithoutDeletingArtifact(t *testing.T) {
	s, now := lifecycleService(t)
	name := lifecycleTask(t, s, lifecycleID, "failed", now, now)
	artifact := lifecycleFile(t, s, lifecycleID, name)
	temporary := lifecycleFile(t, s, lifecycleID, "."+name+".tmp")
	if _, err := s.CleanupOrphans(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatal("inactive export temporary retained", err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatal("owned archive removed", err)
	}
}
