package main

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func commandArchiveFixture(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = auth.NewService(db, auth.Options{}).BootstrapAdministrator(ctx, "archive-admin", "restore-consumer-password"); err != nil {
		t.Fatal(err)
	}
	payload, err := backup.CreateSnapshotPayloads(ctx, dir, filepath.Join(dir, "work"), []backup.Category{backup.Configuration}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	password, err := backup.GeneratePassword(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "archive.tar.gz.age")
	manifest := backup.Manifest{Format: "orbitbeacon-backup", Version: 1, ArchiveID: "70000000-0000-4000-8000-000000000001", CreatedAt: time.Now().UTC(), ServerVersion: "test", SchemaVersion: storage.CurrentSchemaVersion, HistorySchemaVersion: 1, Categories: []backup.Category{backup.Configuration}, Snapshots: payload.Snapshots}
	if _, err = backup.WriteArchive(ctx, archive, manifest, payload.Files, password); err != nil {
		t.Fatal(err)
	}
	passwordFile := filepath.Join(dir, "password.txt")
	if err = os.WriteFile(passwordFile, []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return archive, passwordFile
}

func TestRestoreCommandCreatesUsableLoginAndRequiresReplace(t *testing.T) {
	archive, passwordFile := commandArchiveFixture(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "restored")
	args := []string{"restore", "--data-dir", dir, "--input", archive, "--password-file", passwordFile}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	var mustChangePassword int
	if err = db.QueryRow(`SELECT must_change_password FROM users WHERE username='archive-admin'`).Scan(&mustChangePassword); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if mustChangePassword != 1 {
		db.Close()
		t.Fatal("restored initialized administrator is not required to change password")
	}
	if _, err = auth.NewService(db, auth.Options{}).Login(context.Background(), "archive-admin", "restore-consumer-password"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = run(args); err == nil || err.Error() != "restore_requires_replace" {
		t.Fatalf("replace guard=%v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "orbitbeacon.db"))
	if err != nil || string(before) != string(after) {
		t.Fatal("replace refusal changed target")
	}
	if err = run(append(args, "--replace")); err != nil {
		t.Fatal(err)
	}
}
