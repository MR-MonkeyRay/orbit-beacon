package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestRemoteRestoreJournalRoundTripAndNormalizedTarget(t *testing.T) {
	dir := framingTempDir(t)
	files := make([]restoreOriginal, len(restoreInstallFiles))
	for i, name := range restoreInstallFiles {
		files[i] = restoreOriginal{Name: name, Exists: true, Size: 32, SHA256: strings.Repeat("a", 64)}
	}
	journal := restoreJournal{Version: 2, ID: "11111111-1111-4111-8111-111111111111", State: "prepared", Files: files,
		Remote: &restoreRemote{Target: restoreEndpointFor(restoreConfig(dir)), Metadata: restoreOriginal{Name: restoreMetadataName, Exists: true, Size: 4, SHA256: strings.Repeat("b", 64)}, RollbackID: "22222222-2222-4222-8222-222222222222", NewFiles: files}}
	if err := writeRestoreJournal(context.Background(), dir, journal, osFSOps()); err != nil {
		t.Fatal(err)
	}
	got, err := readRestoreJournal(filepath.Join(dir, restoreJournalName))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, journal) {
		t.Fatalf("journal changed: %+v", got)
	}
	body, err := json.Marshal(restoreConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	var normalized config.Server
	if err = json.Unmarshal(body, &normalized); err != nil {
		t.Fatal(err)
	}
	if restoreEndpointFor(normalized) != got.Remote.Target {
		t.Fatal("normalization changed restore target identity")
	}
}

func TestRemoteRestorePreparedAndCommittedDecisions(t *testing.T) {
	for _, boundary := range []string{"prepared:durable", "restore:pending:durable", "install:main:committed", "install:history:committed", "install:key:durable", "committed:durable", "finish:storage:durable"} {
		t.Run(boundary, func(t *testing.T) {
			dir := restoreFixture(t, "old")
			next := restoreFixture(t, "new")
			cfg := restoreConfig(dir)
			domains, err := storage.OpenOfflineDatabases(t.Context(), cfg.Database, dir)
			if err != nil {
				t.Fatal(err)
			}
			fired := false
			_, err = commitRemoteRestore(t.Context(), cfg, domains, next, journalHook(osFSOps(), func(label string) error {
				if label == boundary && !fired {
					fired = true
					return errors.New("injected")
				}
				return nil
			}))
			if !fired || err == nil {
				t.Fatalf("boundary=%s fired=%t err=%v", boundary, fired, err)
			}
			domains.Close()
			if err = RecoverRestore(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			restored := restoreDB(t, dir)
			defer restored.Close()
			var name string
			if err = restored.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil {
				t.Fatal(err)
			}
			want := "old"
			if boundary == "committed:durable" || boundary == "finish:storage:durable" {
				want = "new"
			}
			if name != want {
				t.Fatalf("got %q want %q", name, want)
			}
			if _, err = os.Stat(filepath.Join(dir, restoreJournalName)); !os.IsNotExist(err) {
				t.Fatalf("journal remains: %v", err)
			}
		})
	}
}

func TestRemoteRestoreCrashesKeepWholeDomains(t *testing.T) {
	for _, boundary := range []string{"prepared:durable", "restore:pending:durable", "install:main:committed", "install:history:committed", "install:key:durable", "committed:durable", "finish:storage:durable"} {
		t.Run(boundary, func(t *testing.T) {
			dir := restoreFixture(t, "target")
			next := restoreFixture(t, "source")
			cfg := restoreConfig(dir)
			originalKey, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
			if err != nil {
				t.Fatal(err)
			}
			defer clear(originalKey)
			configPath := filepath.Join(t.TempDir(), "server.json")
			body, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(configPath, body, 0600); err != nil {
				t.Fatal(err)
			}
			runRemoteCrash(t, configPath, next, boundary, "commit")
			if boundary != "committed:durable" && boundary != "finish:storage:durable" {
				runRemoteCrash(t, configPath, next, "rollback:history:committed", "recover")
			}
			if err = RecoverRestore(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			restored := restoreDB(t, dir)
			var name string
			if err = restored.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil {
				t.Fatal(err)
			}
			restored.Close()
			want, total := "target", float64(22)
			expectedKey := originalKey
			if boundary == "committed:durable" || boundary == "finish:storage:durable" {
				want, total = "source", 11
				expectedKey, err = datadir.LoadKey(filepath.Join(next, "secrets.key"))
				if err != nil {
					t.Fatal(err)
				}
				defer clear(expectedKey)
			}
			if name != want {
				t.Fatalf("name=%q want=%q", name, want)
			}
			metrics, err := storage.OpenReader(context.Background(), filepath.Join(dir, "metrics.db"))
			if err != nil {
				t.Fatal(err)
			}
			var got float64
			err = metrics.QueryRow(`SELECT total FROM buckets WHERE node_id=?`, restoreNode).Scan(&got)
			metrics.Close()
			if err != nil || got != total {
				t.Fatalf("total=%v want=%v err=%v", got, total, err)
			}
			key, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
			if err != nil || !bytes.Equal(key, expectedKey) {
				t.Fatalf("key mismatch: %v", err)
			}
			clear(key)
		})
	}
}

func TestRemoteRestoreMissingRollbackImageDoesNotPartiallyRecover(t *testing.T) {
	dir := restoreFixture(t, "target")
	next := restoreFixture(t, "source")
	cfg := restoreConfig(dir)
	configPath := filepath.Join(t.TempDir(), "server.json")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	runRemoteCrash(t, configPath, next, "install:main:committed", "commit")
	journalPath := filepath.Join(dir, restoreJournalName)
	journal, err := readRestoreJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, ".restore-"+journal.ID, "old", "metrics.db")); err != nil {
		t.Fatal(err)
	}
	if err = RecoverRestore(context.Background(), cfg); restoreCode(err) != "restore_recovery_failed" {
		t.Fatalf("missing image recovery=%v", err)
	}
	if _, err = os.Stat(journalPath); err != nil {
		t.Fatalf("pending journal lost: %v", err)
	}
	main := restoreDB(t, dir)
	defer main.Close()
	var name string
	if err = main.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "source" {
		t.Fatalf("partial rollback name=%q err=%v", name, err)
	}
}

func TestRemoteRestoreRejectsChangedTargetIdentity(t *testing.T) {
	dir := restoreFixture(t, "target")
	next := restoreFixture(t, "source")
	cfg := restoreConfig(dir)
	configPath := filepath.Join(t.TempDir(), "server.json")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	runRemoteCrash(t, configPath, next, "install:main:committed", "commit")
	journalPath := filepath.Join(dir, restoreJournalName)
	journal, err := readRestoreJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal.Remote.MainInstance = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	body, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(journalPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = RecoverRestore(context.Background(), cfg); restoreCode(err) != "restore_recovery_failed" {
		t.Fatalf("changed instance accepted: %v", err)
	}
	if _, err = os.Stat(journalPath); err != nil {
		t.Fatalf("pending decision lost: %v", err)
	}
	main := restoreDB(t, dir)
	defer main.Close()
	var name string
	if err = main.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != "source" {
		t.Fatalf("identity mismatch changed data: name=%q err=%v", name, err)
	}
}

func TestRestoreTargetBindingIncludesStandby(t *testing.T) {
	cfg := restoreConfig(t.TempDir())
	cfg.Database.Standby = &config.DatabaseEndpoint{Driver: "sqlite"}
	remote := &restoreRemote{Target: restoreEndpointFor(cfg), Standby: restoreStandbyFor(cfg)}
	if !restoreTargetMatches(cfg, remote) {
		t.Fatal("same endpoints rejected")
	}
	cfg.Database.Standby = nil
	if restoreTargetMatches(cfg, remote) {
		t.Fatal("removed standby accepted during recovery")
	}
	cfg.Database.Standby = &config.DatabaseEndpoint{Driver: "mysql", Address: "changed.example:3306"}
	if restoreTargetMatches(cfg, remote) {
		t.Fatal("changed standby accepted during recovery")
	}
}
