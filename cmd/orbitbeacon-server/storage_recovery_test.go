package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func storageCommandOutput(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "stdout-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	old := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = old }()
	commandErr := run(args)
	if _, err = output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	return raw, commandErr
}

func TestStorageStatusDoesNotInitializeDatabasesOrTakeServerLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	raw, err := storageCommandOutput(t, "storage", "status", "--data-dir", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var status storage.RecoveryStatus
	if err = json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status.ObservedAt == nil || status.ObservedAt.IsZero() || status.Enabled || len(status.Domains) != 2 {
		t.Fatalf("invalid local observation: %+v", status)
	}
	for _, name := range []string{"storage-control.db", "orbitbeacon.db", "metrics.db"} {
		if _, err = os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("status initialized %s: %v", name, err)
		}
	}
}

func TestStorageRecoveryCommandsRequireServerLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for _, args := range [][]string{
		{"storage", "preview", "--data-dir", dir, "--domain", "main", "--action", "rebuild", "--target", "standby"},
		{"storage", "run", "--data-dir", dir, "--operation", "8bcb40ab-85d9-4521-bb2d-fcc1b5ea0c52", "--confirm", "main/standby/8bcb40ab-85d9-4521-bb2d-fcc1b5ea0c52"},
	} {
		if _, err = storageCommandOutput(t, args...); !errors.Is(err, datadir.ErrInUse) {
			t.Fatalf("locked command reached recovery: %v", err)
		}
	}
}
