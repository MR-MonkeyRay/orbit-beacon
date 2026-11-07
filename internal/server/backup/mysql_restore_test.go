package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLBackupRestore(t *testing.T) {
	fixture := storagetest.Start(t)
	ctx := context.Background()
	source := restoreFixture(t, "new")
	sourceDB := writableRestoreFixture(t, source)
	at := "2026-10-01T00:00:00.000000000Z"
	if _, err := sourceDB.Exec(`INSERT INTO inbox_notifications(sequence,id,kind,source_id,transition,severity,created_at,details_json) VALUES(17,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','alert','imported-one','resolved','info',?,'{}'),(18,'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','alert','imported-two','resolved','info',?,'{}')`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceDB.Exec(`INSERT INTO inbox_reads(user_id,notification_id,read_at) VALUES(?,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',?)`, restoreUser, at); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceDB.Exec(`UPDATE inbox_sequence SET value=18 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	sourceDB.Close()
	archive, password := restoreArchive(t, source, []Category{Configuration, Monitoring, Events})
	installSource := framingTempDir(t)
	for _, name := range restoreInstallFiles {
		version := storage.CurrentSchemaVersion
		if name == "metrics.db" {
			version = storage.HistorySchemaVersion
		}
		if name == "secrets.key" {
			if err := copyRestoreFile(ctx, filepath.Join(source, name), filepath.Join(installSource, name)); err != nil {
				t.Fatal(err)
			}
		} else if err := SnapshotDatabase(ctx, filepath.Join(source, name), filepath.Join(installSource, name), version); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := fixture.Endpoint("primary")
	main := fixture.Open(t, storage.MainDomain)
	metrics := fixture.Open(t, storage.HistoryDomain)
	reset := func(t *testing.T) config.Server {
		t.Helper()
		dir := restoreFixture(t, "old")
		oldHistory, err := storage.OpenHistorySQLite(ctx, filepath.Join(dir, "metrics.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = oldHistory.Exec(`UPDATE buckets SET total=22,peak=22`); err != nil {
			t.Fatal(err)
		}
		oldHistory.Close()
		if err := storage.ReplaceDomain(ctx, main, filepath.Join(dir, "orbitbeacon.db"), uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if err := storage.ReplaceDomain(ctx, metrics, filepath.Join(dir, "metrics.db"), uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		return config.Server{DataDir: dir, Database: config.DatabaseConfig{Primary: endpoint}}
	}
	assertState := func(t *testing.T, cfg config.Server, want string) {
		t.Helper()
		var name string
		if err := fixture.Raw(t, storage.MainDomain).QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != want {
			t.Fatalf("name=%q want=%q err=%v", name, want, err)
		}
		var count int
		var total float64
		if err := fixture.Raw(t, storage.HistoryDomain).QueryRow(`SELECT count(*),sum(total) FROM buckets WHERE node_id=?`, restoreNode).Scan(&count, &total); err != nil || count != 1 {
			t.Fatalf("history=%d err=%v", count, err)
		}
		expected := float64(22)
		if want == "new" {
			expected = 11
		}
		if total != expected {
			t.Fatalf("history total=%v want=%v", total, expected)
		}
	}
	t.Run("active_snapshot_and_restore", func(t *testing.T) {
		cfg := reset(t)
		// 本地 SQLite 仍为 old，备份必须读取真实 MySQL，而非旁边的本地文件。
		lease, err := main.Acquire(ctx, storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			lease.Close()
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(lease.Context(), `UPDATE nodes SET name='remote' WHERE id=?`, restoreNode); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		lease.Close()
		work := framingTempDir(t)
		payloads, err := CreateSnapshotPayloads(ctx, SnapshotSources{Main: main, History: metrics}, cfg.DataDir, work, []Category{Configuration, Monitoring, Events}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := openPayload(ctx, filepath.Join(work, "configuration.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		var name string
		if err = payload.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "remote" {
			t.Fatalf("snapshot name=%q err=%v", name, err)
		}
		payload.Close()
		if len(payloads.Files) != 4 {
			t.Fatalf("payload files=%d", len(payloads.Files))
		}
		if _, err = Restore(ctx, cfg, archive, password, false); restoreCode(err) != "restore_requires_replace" {
			t.Fatalf("nonempty restore=%v", err)
		}
		result, err := Restore(ctx, cfg, archive, password, true)
		if err != nil || result.RollbackPath == "" {
			t.Fatalf("restore=%+v err=%v", result, err)
		}
		assertState(t, cfg, "new")
		var quota, generation string
		var token []byte
		if err = fixture.Raw(t, storage.MainDomain).QueryRow(`SELECT quota_bytes,run_generation,token_hash FROM nodes WHERE id=?`, restoreNode).Scan(&quota, &generation, &token); err != nil || quota != "18446744073709551615" || generation != "17" || !bytes.Equal(token, bytes.Repeat([]byte{3}, 32)) {
			t.Fatalf("logical types quota=%q generation=%q token_len=%d err=%v", quota, generation, len(token), err)
		}
		var maximum int64
		if err = fixture.Raw(t, storage.MainDomain).QueryRow(`SELECT max(sequence) FROM inbox_notifications`).Scan(&maximum); err != nil {
			t.Fatal(err)
		}
		var reads int
		if err = fixture.Raw(t, storage.MainDomain).QueryRow(`SELECT count(*) FROM inbox_reads WHERE user_id=? AND notification_id='aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'`, restoreUser).Scan(&reads); err != nil || reads != 1 {
			t.Fatalf("read relation=%d err=%v", reads, err)
		}
		// 归档恢复后的计数器必须能继续分配；真实业务 Insert 由包内统一调用。
		lease, err = main.Acquire(ctx, storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
		tx, err = lease.BeginWrite(lease.Context())
		if err != nil {
			lease.Close()
			t.Fatal(err)
		}
		if err = insertRestoredNotification(lease.Context(), tx); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		lease.Close()
		var sequence, counter int64
		if err = fixture.Raw(t, storage.MainDomain).QueryRow(`SELECT sequence FROM inbox_notifications WHERE source_id='post-restore'`).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		if err = fixture.Raw(t, storage.MainDomain).QueryRow(`SELECT value FROM inbox_sequence WHERE id=1`).Scan(&counter); err != nil || sequence != counter || sequence <= maximum {
			t.Fatalf("sequence=%d max=%d counter=%d err=%v", sequence, maximum, counter, err)
		}
		if err = inbox.New(main).MarkAllRead(ctx, inbox.Identity{UserID: restoreUser}, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err = inbox.New(main).MarkAllRead(ctx, inbox.Identity{UserID: restoreUser}, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", time.Now()); !errors.Is(err, inbox.ErrNotFound) {
			t.Fatalf("missing through=%v", err)
		}
	})
	t.Run("worker_exports_active_domains", func(t *testing.T) {
		cfg := reset(t)
		key, err := datadir.LoadKey(filepath.Join(cfg.DataDir, "secrets.key"))
		if err != nil {
			t.Fatal(err)
		}
		box, err := secretbox.New(key, "orbitbeacon backup secrets v1")
		clear(key)
		if err != nil {
			t.Fatal(err)
		}
		service, err := New(main, Options{DataDir: cfg.DataDir, History: metrics, SecretBox: box, ServerVersion: "test"})
		if err != nil {
			t.Fatal(err)
		}
		task, err := service.CreateExport(ctx, restoreUser, []Category{Configuration, Monitoring, Events})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := service.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if claimed.ID != task.ID {
			t.Fatalf("claimed %q want %q", claimed.ID, task.ID)
		}
		service.execute(ctx, claimed)
		finished, err := service.loadTask(ctx, task.ID)
		if err != nil || finished.State != "succeeded" || finished.ResultLocation == nil {
			t.Fatalf("finished=%+v err=%v", finished, err)
		}
		password, err := box.Open("task-password:"+task.ID, finished.passwordCiphertext)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(password)
		output := filepath.Join(framingTempDir(t), "archive")
		if _, err = ReadArchive(ctx, *finished.ResultLocation, string(password), output); err != nil {
			t.Fatal(err)
		}
		payload, err := openPayload(ctx, filepath.Join(output, "monitoring.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer payload.Close()
		var total float64
		if err = payload.QueryRow(`SELECT total FROM buckets WHERE node_id=?`, restoreNode).Scan(&total); err != nil || total != 22 {
			t.Fatalf("worker history total=%v err=%v", total, err)
		}
		if _, err = service.Cleanup(ctx, time.Now().Add(100*24*time.Hour), 500); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err = fixture.Raw(t, storage.MainDomain).QueryRow(`SELECT count(*) FROM backup_tasks WHERE id=?`, task.ID).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("cleanup tasks=%d err=%v", remaining, err)
		}
	})
	t.Run("unavailable_recovery_keeps_prepared", func(t *testing.T) {
		cfg := reset(t)
		configPath := filepath.Join(t.TempDir(), "server.json")
		body, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(configPath, body, 0600); err != nil {
			t.Fatal(err)
		}
		runRemoteCrash(t, configPath, installSource, "install:main:committed", "commit")
		journalPath := filepath.Join(cfg.DataDir, restoreJournalName)
		before, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		wrong := cfg
		wrong.Database.Primary.PasswordFile = filepath.Join(t.TempDir(), "bad-password")
		if err = os.WriteFile(wrong.Database.Primary.PasswordFile, []byte("not-the-database-password"), 0600); err != nil {
			t.Fatal(err)
		}
		if err = RecoverRestore(ctx, wrong); err == nil {
			t.Fatal("unavailable target allowed startup")
		}
		after, err := os.ReadFile(journalPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("pending decision changed: %v", err)
		}
		if err = RecoverRestore(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		assertState(t, cfg, "old")
	})
	for _, boundary := range []string{"prepared:durable", "install:main:committed", "install:history:committed", "install:key:durable", "committed:durable"} {
		t.Run(boundary, func(t *testing.T) {
			cfg := reset(t)
			configPath := filepath.Join(t.TempDir(), "server.json")
			body, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(configPath, body, 0600); err != nil {
				t.Fatal(err)
			}
			runRemoteCrash(t, configPath, installSource, boundary, "commit")
			if boundary != "committed:durable" {
				runRemoteCrash(t, configPath, installSource, "rollback:main:committed", "recover")
			}
			if err = RecoverRestore(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			want := "old"
			if boundary == "committed:durable" {
				want = "new"
			}
			assertState(t, cfg, want)
			expectedDir := cfg.DataDir
			if want == "new" {
				expectedDir = installSource
			}
			got, err := datadir.LoadKey(filepath.Join(cfg.DataDir, "secrets.key"))
			if err != nil {
				t.Fatal(err)
			}
			defer clear(got)
			var expected []byte
			if want == "old" {
				journalDirs, err := filepath.Glob(filepath.Join(cfg.DataDir, ".restore-*", "old", "secrets.key"))
				if err != nil || len(journalDirs) != 1 {
					t.Fatalf("rollback key: %v %v", journalDirs, err)
				}
				expected, err = datadir.LoadKey(journalDirs[0])
			} else {
				expected, err = datadir.LoadKey(filepath.Join(expectedDir, "secrets.key"))
			}
			if err != nil || !bytes.Equal(got, expected) {
				t.Fatalf("key mismatch: %v", err)
			}
			clear(expected)
		})
	}
	t.Run("fresh_mysql_restore_without_local_database", func(t *testing.T) {
		dir := framingTempDir(t)
		cfg := config.Server{DataDir: dir, Database: config.DatabaseConfig{Primary: fixture.Endpoint("standby")}}
		events, eventsPassword := restoreArchive(t, source, []Category{Events})
		if _, err := Restore(ctx, cfg, events, eventsPassword, false); restoreCode(err) != "configuration_required" {
			t.Fatalf("empty remote restore=%v", err)
		}
		if _, err := Restore(ctx, cfg, archive, password, false); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"orbitbeacon.db", "metrics.db"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("remote restore created local %s: %v", name, err)
			}
		}
		domains, err := storage.OpenDatabases(ctx, cfg.Database, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer domains.Close()
		lease, err := domains.Main.Acquire(ctx, storage.Reader)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		var name string
		if err = lease.QueryRowContext(lease.Context(), `SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "new" {
			t.Fatalf("fresh MySQL name=%q err=%v", name, err)
		}
	})
}

func runRemoteCrash(t *testing.T, cfg, next, label, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemoteRestoreCrashHelper$")
	cmd.Env = append(os.Environ(), "OB_REMOTE_CHILD=1", "OB_REMOTE_CONFIG="+cfg, "OB_REMOTE_NEXT="+next, "OB_REMOTE_LABEL="+label, "OB_REMOTE_MODE="+mode)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("crash %s not reached: %v %s", label, err, out)
	}
}

func TestRemoteRestoreCrashHelper(t *testing.T) {
	if os.Getenv("OB_REMOTE_CHILD") != "1" {
		return
	}
	body, err := os.ReadFile(os.Getenv("OB_REMOTE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Server
	if err = json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	lock, err := datadir.Lock(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ops := journalHook(osFSOps(), func(label string) error {
		if label == os.Getenv("OB_REMOTE_LABEL") {
			os.Exit(77)
		}
		return nil
	})
	if os.Getenv("OB_REMOTE_MODE") == "recover" {
		journal, err := readRestoreJournal(filepath.Join(cfg.DataDir, restoreJournalName))
		if err != nil {
			t.Fatal(err)
		}
		err = recoverRemoteRestore(context.Background(), cfg, journal, ops)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		domains, err := storage.OpenDatabases(context.Background(), cfg.Database, cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer domains.Close()
		if _, err = commitRemoteRestore(context.Background(), cfg, domains, os.Getenv("OB_REMOTE_NEXT"), ops); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("crash point not reached")
}

func insertRestoredNotification(ctx context.Context, tx *storage.Tx) error {
	return inbox.Insert(ctx, tx, inbox.Event{Kind: "alert", SourceID: "post-restore", Transition: "resolved", Severity: "info", CreatedAt: time.Now().UTC(), Details: json.RawMessage(`{}`)})
}
