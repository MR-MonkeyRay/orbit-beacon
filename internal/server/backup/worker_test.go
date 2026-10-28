package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingUploader struct {
	calls   atomic.Int32
	mu      sync.Mutex
	ids     []string
	result  UploadResult
	err     error
	inspect func(context.Context, string, string) error
}

func (u *recordingUploader) Upload(ctx context.Context, d ObjectDestination, c ObjectCredentials, path, key, hash string) (UploadResult, error) {
	u.calls.Add(1)
	u.mu.Lock()
	u.ids = append(u.ids, key)
	u.mu.Unlock()
	if u.inspect != nil {
		if err := u.inspect(ctx, path, key); err != nil {
			return UploadResult{}, err
		}
	}
	return u.result, u.err
}
func enqueueRemote(t *testing.T, s *Service, id string, created time.Time) {
	t.Helper()
	creds, err := json.Marshal(ObjectCredentials{AccessKeyID: "test-id", SecretAccessKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := s.box.Seal("task-input:"+id, creds)
	clear(creds)
	if err != nil {
		t.Fatal(err)
	}
	destination := `{"plan_name":"Remote","actor_name":"","target":{"provider":"s3_compatible","endpoint":"https://storage.example.com","region":"test","bucket":"test","prefix":"","path_style":true,"allow_private_networks":false}}`
	_, err = s.db.Exec(`INSERT INTO backup_tasks(id,kind,state,plan_id,categories_json,destination_json,input_credentials_ciphertext,created_at) VALUES(?,'backup_remote','queued',?,'["configuration"]',?,?,?)`, id, id, destination, cipher, created.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		t.Fatal(err)
	}
}
func runWorker(t *testing.T, s *Service) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return cancel
}

func TestWorkerRealArchiveSurvivesRequestCancelAndUsesTaskArchiveID(t *testing.T) {
	db, opts := workerFixture(t)
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	req, cancel := context.WithCancel(context.Background())
	task, err := s.CreateExport(req, restoreUser, []Category{Configuration})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	runWorker(t, s)
	waitWorkerTask(t, db, task.ID, "succeeded")
	task, err = s.loadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ArtifactName == nil || !strings.Contains(*task.ArtifactName, task.ID) || task.ResultLocation == nil || task.PasswordExpiresAt == nil || task.ArtifactExpiresAt == nil {
		t.Fatalf("missing terminal metadata: %+v", task)
	}
	plain, err := s.box.Open("task-password:"+task.ID, task.passwordCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plain)
	work := framingTempDir(t)
	manifest, err := ReadArchive(context.Background(), *task.ResultLocation, string(plain), work)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ArchiveID != task.ID {
		t.Fatal("second archive identity")
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM inbox_notifications WHERE source_id=? AND transition='succeeded'`, task.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("notification=%d %v", n, err)
	}
	entries, err := os.ReadDir(filepath.Join(opts.DataDir, "backup-work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("private staging retained")
	}
	entries, err = os.ReadDir(filepath.Dir(*task.ResultLocation))
	if err != nil || len(entries) != 1 {
		t.Fatalf("extra plaintext/temp artifact: %v %v", entries, err)
	}
}

func TestWorkerPublicationOnceAcrossAtomicFinalizationFailures(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "inbox_rollback", true: "commit_outcome"}[uncertain], func(t *testing.T) {
			db, opts := workerFixture(t)
			u := &recordingUploader{}
			opts.Uploader = u
			var attempts atomic.Int32
			if uncertain {
				opts.afterTerminalCommit = func() error {
					if attempts.Add(1) == 1 {
						return errors.New("commit response lost")
					}
					return nil
				}
			} else {
				opts.beforeTerminal = func(tx *sql.Tx) error {
					if attempts.Add(1) == 1 {
						var state string
						var n int
						if err := tx.QueryRow(`SELECT state FROM backup_tasks WHERE kind='backup_remote'`).Scan(&state); err != nil {
							return err
						}
						if err := tx.QueryRow(`SELECT count(*) FROM inbox_notifications WHERE kind='task'`).Scan(&n); err != nil {
							return err
						}
						if state != "succeeded" || n != 1 {
							return errors.New("terminal and inbox not in same transaction")
						}
						return errors.New("inbox fault")
					}
					return nil
				}
			}
			s, err := New(db, opts)
			if err != nil {
				t.Fatal(err)
			}
			id := "10000000-0000-4000-8000-000000000001"
			enqueueRemote(t, s, id, time.Now())
			runWorker(t, s)
			waitWorkerTask(t, db, id, "succeeded")
			if u.calls.Load() != 1 {
				t.Fatalf("external publish count=%d", u.calls.Load())
			}
			var n int
			if err = db.QueryRow(`SELECT count(*) FROM inbox_notifications WHERE source_id=?`, id).Scan(&n); err != nil || n != 1 {
				t.Fatalf("notification=%d %v", n, err)
			}
		})
	}
}

func TestWorkerRestartPreservesQueuedAndFailsRunningWithoutRepublish(t *testing.T) {
	db, opts := workerFixture(t)
	u := &recordingUploader{}
	opts.Uploader = u
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	id := "10000000-0000-4000-8000-000000000002"
	cipher, err := s.box.Seal("task-password:"+id, []byte("ABCDEFGHIJKLMNOP"))
	if err != nil {
		t.Fatal(err)
	}
	insertWorkerTask(t, db, id, "backup_export", "running", "storing", `{}`, cipher)
	_, err = db.Exec(`UPDATE backup_tasks SET artifact_name='archive.tar.gz.age',artifact_bytes=123,artifact_sha256=?,result_location='/not/readable' WHERE id=?`, strings.Repeat("a", 64), id)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := s.CreateExport(context.Background(), restoreUser, []Category{Configuration})
	if err != nil {
		t.Fatal(err)
	}
	runWorker(t, s)
	waitWorkerTask(t, db, id, "failed")
	waitWorkerTask(t, db, queued.ID, "succeeded")
	task, err := s.loadTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if task.ErrorCode != "interrupted" || task.WarningCode != "storage_outcome_unknown" || task.PasswordExpiresAt == nil || len(task.passwordCiphertext) == 0 || len(task.inputCredentialsCiphertext) != 0 {
		t.Fatalf("restart metadata: %+v", task)
	}
	if u.calls.Load() != 0 {
		t.Fatal("interrupted upload replayed")
	}
}

func TestWorkerRemoteFIFOAndFailureClassification(t *testing.T) {
	db, opts := workerFixture(t)
	u := &recordingUploader{result: UploadResult{OutcomeUnknown: true}, err: &Error{Code: "storage_upload_failed"}}
	opts.Uploader = u
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := "10000000-0000-4000-8000-000000000004"
	second := "10000000-0000-4000-8000-000000000003"
	enqueueRemote(t, s, first, time.Now().Add(-time.Minute))
	enqueueRemote(t, s, second, time.Now())
	u.inspect = func(ctx context.Context, path, key string) error {
		var phase string
		var password []byte
		var expiry sql.NullString
		var active int
		if err := db.QueryRowContext(ctx, `SELECT phase,password_ciphertext,password_expires_at FROM backup_tasks WHERE state='running'`).Scan(&phase, &password, &expiry); err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM backup_tasks WHERE state='running'`).Scan(&active); err != nil {
			return err
		}
		if phase != "storing" || len(password) == 0 || expiry.Valid || active != 1 {
			return errors.New("unsafe publication preconditions")
		}
		return nil
	}
	runWorker(t, s)
	waitWorkerTask(t, db, second, "failed")
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.ids) != 2 || !strings.Contains(u.ids[0], first) || !strings.Contains(u.ids[1], second) {
		t.Fatalf("non FIFO: %v", u.ids)
	}
	for _, id := range []string{first, second} {
		task, err := s.loadTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if task.ErrorCode != "storage_upload_failed" || task.WarningCode != "storage_outcome_unknown" || task.PasswordExpiresAt == nil {
			t.Fatalf("failure projection: %+v", task)
		}
	}
}

func TestWorkerRemoteResultIdentifiesObjectKeyWithoutInventingURL(t *testing.T) {
	db, opts := workerFixture(t)
	opts.Uploader = &recordingUploader{}
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	id := "10000000-0000-4000-8000-000000000099"
	enqueueRemote(t, s, id, time.Now())
	destination := `{"plan_name":"OSS custom domain","actor_name":"","target":{"provider":"oss","endpoint":"https://bound.example.com","region":"cn-hangzhou","bucket":"test","prefix":"daily","path_style":false,"allow_private_networks":false}}`
	if _, err = db.Exec(`UPDATE backup_tasks SET destination_json=? WHERE id=?`, destination, id); err != nil {
		t.Fatal(err)
	}
	runWorker(t, s)
	waitWorkerTask(t, db, id, "succeeded")
	task, err := s.Task(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := "daily/orbitbeacon/" + id + "/" + *task.ArtifactName
	if task.ResultLocation == nil || *task.ResultLocation != want {
		t.Fatalf("object key = %v, want %q", task.ResultLocation, want)
	}
	var snapshot string
	if err = db.QueryRow(`SELECT details_json FROM inbox_notifications WHERE source_id=?`, id).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var notification Task
	if err = json.Unmarshal([]byte(snapshot), &notification); err != nil {
		t.Fatal(err)
	}
	if notification.ResultLocation == nil || *notification.ResultLocation != want {
		t.Fatalf("notification object key = %v, want %q", notification.ResultLocation, want)
	}
}

func TestWorkerStartupCleansOnlyInactiveOwnedStaging(t *testing.T) {
	db, opts := workerFixture(t)
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(opts.DataDir, "backup-work")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := "10000000-0000-4000-8000-000000000009"
	old := filepath.Join(root, orphan)
	if err = os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(old, "main-snapshot.sqlite"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	queued, err := s.CreateExport(context.Background(), restoreUser, []Category{Configuration})
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, queued.ID)
	if err = os.Mkdir(active, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "unrelated")
	if err = os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err = s.cleanupWork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("inactive staging retained")
	}
	for _, p := range []string{active, foreign} {
		if _, err = os.Stat(p); err != nil {
			t.Fatal("active/unowned staging removed")
		}
	}
}

func TestWorkerExplicitRemoteRejectionDoesNotClaimUnknownOutcome(t *testing.T) {
	db, opts := workerFixture(t)
	opts.Uploader = &recordingUploader{err: &Error{Code: "storage_auth_failed", Err: errors.New("https://secret-user:secret-pass@storage.invalid")}}
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	id := "10000000-0000-4000-8000-000000000010"
	enqueueRemote(t, s, id, time.Now())
	runWorker(t, s)
	waitWorkerTask(t, db, id, "failed")
	task, err := s.loadTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if task.ErrorCode != "storage_auth_failed" || task.WarningCode != "" {
		t.Fatalf("explicit rejection misclassified: %+v", task)
	}
	var details string
	if err = db.QueryRow(`SELECT details_json FROM inbox_notifications WHERE source_id=?`, id).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(details, "secret-pass") || strings.Contains(details, "secret-user") {
		t.Fatal("unsafe storage error escaped")
	}
}
