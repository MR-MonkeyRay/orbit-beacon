package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func TestResetTOTPCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := auth.NewService(db, auth.Options{})
	user, err := s.CreateUser(ctx, "rescue-admin", "rescue-password", false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Login(ctx, user.Username, "rescue-password", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE users SET totp_ciphertext=x'01',totp_pending_ciphertext=x'02',totp_pending_at='2026-11-01T00:00:00.000000000Z',totp_last_step=123 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO recovery_codes VALUES(?,zeroblob(32))`, user.ID); err != nil {
		t.Fatal(err)
	}
	args := []string{"reset-totp", "--data-dir", dir, "--username", user.Username}
	lock, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = run(args)
	lock.Close()
	if !errors.Is(err, datadir.ErrInUse) {
		t.Fatalf("locked reset: %v", err)
	}
	var enabled bool
	db.QueryRow(`SELECT totp_ciphertext IS NOT NULL FROM users WHERE id=?`, user.ID).Scan(&enabled)
	if !enabled {
		t.Fatal("locked command changed user")
	}
	if err = run(args); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT (SELECT count(*) FROM recovery_codes)+(SELECT count(*) FROM sessions)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("credentials retained", count, err)
	}
	if _, err = s.CurrentSession(ctx, session.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal(err)
	}
	var auditCount int
	db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='user.totp.reset' AND actor_id IS NULL AND json_extract(details,'$.actor_kind')='reset-totp'`).Scan(&auditCount)
	if auditCount != 1 {
		t.Fatal("system audit missing")
	}
	if err = run([]string{"reset-totp", "--data-dir", dir, "--username", "missing"}); !errors.Is(err, auth.ErrNotFound) {
		t.Fatal("missing user", err)
	}
}

func enrolledDataDir(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := auth.NewService(db, auth.Options{}).CreateUser(ctx, "journal-admin", "journal-password", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE users SET totp_ciphertext=x'01' WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO recovery_codes VALUES(?,zeroblob(32))`, user.ID); err != nil {
		t.Fatal(err)
	}
	return dir, user.Username
}

func secondFactorCount(t *testing.T, dir string) int {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT (SELECT count(*) FROM users WHERE totp_ciphertext IS NOT NULL)+(SELECT count(*) FROM recovery_codes)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// 崩溃遗留的 prepared 日志必须先收敛。否则下一次启动会回滚文件组，撤销这次重置。
func TestResetTOTPSurvivesCrashedRestoreJournal(t *testing.T) {
	dir, username := enrolledDataDir(t)
	const id = "70000000-0000-4000-8000-000000000002"
	old := filepath.Join(dir, ".restore-"+id, "old")
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	type file struct {
		Name   string `json:"name"`
		Exists bool   `json:"exists"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	journal := struct {
		Version int    `json:"version"`
		ID      string `json:"id"`
		State   string `json:"state"`
		Files   []file `json:"files"`
	}{Version: 1, ID: id, State: "prepared"}
	for _, name := range []string{"orbitbeacon.db", "orbitbeacon.db-wal", "orbitbeacon.db-shm", "orbitbeacon.db-journal", "metrics.db", "metrics.db-wal", "metrics.db-shm", "metrics.db-journal", "secrets.key"} {
		entry := file{Name: name}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			sum := sha256.Sum256(body)
			entry = file{Name: name, Exists: true, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
			if err = os.WriteFile(filepath.Join(old, name), body, 0600); err != nil {
				t.Fatal(err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		journal.Files = append(journal.Files, entry)
	}
	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "restore-journal.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"reset-totp", "--data-dir", dir, "--username", username}); err != nil {
		t.Fatal(err)
	}
	if err = backup.RecoverRestore(dir); err != nil {
		t.Fatal(err)
	}
	if count := secondFactorCount(t, dir); count != 0 {
		t.Fatalf("restart restored %d second factor rows", count)
	}
}

func TestResetTOTPStopsOnUnrecoverableRestoreJournal(t *testing.T) {
	dir, username := enrolledDataDir(t)
	if err := os.WriteFile(filepath.Join(dir, "restore-journal.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"reset-totp", "--data-dir", dir, "--username", username})
	var failure *backup.Error
	if !errors.As(err, &failure) || failure.Code != "restore_recovery_failed" {
		t.Fatal(err)
	}
	if count := secondFactorCount(t, dir); count != 2 {
		t.Fatalf("failed recovery changed second factor rows: %d", count)
	}
}

func TestResetTOTPDoesNotCreateDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"reset-totp", "--data-dir", dir, "--username", "admin"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "orbitbeacon.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestResetTOTPCommandArguments(t *testing.T) {
	for _, args := range [][]string{{"reset-totp"}, {"reset-totp", "--data-dir", "/unused"}, {"reset-totp", "--username", "admin"}, {"reset-totp", "--unknown"}, {"reset-totp", "extra"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := run([]string{"reset-totp", "--help"}); err != nil {
		t.Fatal(err)
	}
}
