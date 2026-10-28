package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/datadir"
)

func journalFixture(t *testing.T) (string, string, map[string][]byte, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	data, next := filepath.Join(root, "data"), filepath.Join(root, "next")
	for _, path := range []string{data, next} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	old, fresh := map[string][]byte{}, map[string][]byte{}
	for _, name := range restoreFiles {
		old[name] = []byte("old:" + name)
		if name == "secrets.key" {
			old[name] = bytes.Repeat([]byte{1}, 32)
		}
		if err := os.WriteFile(filepath.Join(data, name), old[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range restoreInstallFiles {
		fresh[name] = []byte("new:" + name)
		if name == "secrets.key" {
			fresh[name] = bytes.Repeat([]byte{2}, 32)
		}
		if err := os.WriteFile(filepath.Join(next, name), fresh[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	return data, next, old, fresh
}

func journalAssertFiles(t *testing.T, data string, want map[string][]byte) {
	t.Helper()
	for _, name := range restoreFiles {
		got, err := os.ReadFile(filepath.Join(data, name))
		expected, exists := want[name]
		if !exists {
			if !os.IsNotExist(err) {
				t.Fatalf("unexpected %s: %v", name, err)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("mixed state %s: got %q (%v), want %q", name, got, err, expected)
		}
		info, err := os.Lstat(filepath.Join(data, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe file %s: %v", name, err)
		}
	}
}

func journalHook(ops fsOps, hook func(string) error) fsOps {
	copyFile, fileSync, dirSync, rename, remove, stage := ops.copy, ops.fileSync, ops.dirSync, ops.rename, ops.remove, ops.journal
	ops.copy = func(label, src, dst string) error {
		if err := hook(label); err != nil {
			return err
		}
		return copyFile(label, src, dst)
	}
	ops.fileSync = func(label string, file *os.File) error {
		if err := hook(label); err != nil {
			return err
		}
		return fileSync(label, file)
	}
	ops.dirSync = func(label, path string) error {
		if err := hook(label); err != nil {
			return err
		}
		return dirSync(label, path)
	}
	ops.rename = func(label, src, dst string) error {
		if err := hook(label); err != nil {
			return err
		}
		return rename(label, src, dst)
	}
	ops.remove = func(label, path string) error {
		if err := hook(label); err != nil {
			return err
		}
		return remove(label, path)
	}
	ops.journal = func(label string) error {
		if err := hook(label); err != nil {
			return err
		}
		return stage(label)
	}
	return ops
}

func TestRestoreJournalCommit(t *testing.T) {
	data, next, old, fresh := journalFixture(t)
	lock, err := datadir.Lock(data)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := CommitRestore(context.Background(), data, next)
	if err != nil {
		t.Fatal(err)
	}
	journalAssertFiles(t, data, fresh)
	journalAssertFiles(t, rollback, old)
	after, err := os.Stat(filepath.Join(data, "server.lock"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("server.lock inode changed")
	}
	if _, err := os.Stat(filepath.Join(data, restoreJournalName)); !os.IsNotExist(err) {
		t.Fatalf("journal remains: %v", err)
	}
	if err := RecoverRestore(data); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreJournalAbsentOriginalAndCancellation(t *testing.T) {
	data, next, old, _ := journalFixture(t)
	for _, name := range []string{"metrics.db", "orbitbeacon.db-wal", "secrets.key"} {
		if err := os.Remove(filepath.Join(data, name)); err != nil {
			t.Fatal(err)
		}
		delete(old, name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CommitRestore(ctx, data, next); err == nil {
		t.Fatal("canceled commit succeeded")
	}
	journalAssertFiles(t, data, old)
	journalCrash(t, "commit", "prepared:durable", data, next)
	if err := RecoverRestore(data); err != nil {
		t.Fatal(err)
	}
	journalAssertFiles(t, data, old)
}

func journalCrash(t *testing.T, mode, label, data, next string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestoreJournalCrashHelper$")
	cmd.Env = append(os.Environ(), "OB_JOURNAL_CHILD=1", "OB_JOURNAL_MODE="+mode, "OB_JOURNAL_LABEL="+label, "OB_JOURNAL_DATA="+data, "OB_JOURNAL_NEXT="+next)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("crash label %q not reached: %v %s", label, err, out)
	}
}

func TestRestoreJournalCrashHelper(t *testing.T) {
	if os.Getenv("OB_JOURNAL_CHILD") != "1" {
		return
	}
	ops := journalHook(osFSOps(), func(label string) error {
		if label == os.Getenv("OB_JOURNAL_LABEL") {
			os.Exit(77)
		}
		return nil
	})
	if os.Getenv("OB_JOURNAL_MODE") == "recover" {
		_ = recoverRestore(os.Getenv("OB_JOURNAL_DATA"), ops)
	} else {
		_, _ = commitRestore(context.Background(), os.Getenv("OB_JOURNAL_DATA"), os.Getenv("OB_JOURNAL_NEXT"), ops)
	}
	t.Fatal("crash point not reached")
}

func TestRestoreJournalEveryCommitOperation(t *testing.T) {
	data, next, _, _ := journalFixture(t)
	var labels []string
	_, err := commitRestore(context.Background(), data, next, journalHook(osFSOps(), func(label string) error { labels = append(labels, label); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	committedPublished := false
	for _, label := range labels {
		if seen[label] {
			t.Fatalf("nonunique operation label %s", label)
		}
		seen[label] = true
		expectNew := committedPublished
		if label == "committed:rename" {
			committedPublished = true
		}
		for _, crash := range []bool{false, true} {
			t.Run(label+map[bool]string{false: "/error", true: "/exit"}[crash], func(t *testing.T) {
				data, next, old, fresh := journalFixture(t)
				if crash {
					journalCrash(t, "commit", label, data, next)
				} else {
					fired := false
					_, err := commitRestore(context.Background(), data, next, journalHook(osFSOps(), func(at string) error {
						if at == label && !fired {
							fired = true
							return errors.New("injected")
						}
						return nil
					}))
					if err == nil || !fired {
						t.Fatalf("fault not propagated: %v", err)
					}
					var safe *Error
					if !errors.As(err, &safe) || safe.Code != "restore_commit_failed" || strings.Contains(err.Error(), "injected") {
						t.Fatalf("unsafe error: %v", err)
					}
				}
				// Hooks run before each operation; only a completed committed
				// journal rename authorizes the complete new group.
				committed := expectNew
				if err := RecoverRestore(data); err != nil {
					t.Fatal(err)
				}
				if committed {
					journalAssertFiles(t, data, fresh)
				} else {
					journalAssertFiles(t, data, old)
				}
			})
		}
	}
}

func TestRestoreJournalEveryRecoveryOperation(t *testing.T) {
	data, next, _, _ := journalFixture(t)
	journalCrash(t, "commit", "install:metrics.db:rename", data, next)
	var labels []string
	if err := recoverRestore(data, journalHook(osFSOps(), func(label string) error { labels = append(labels, label); return nil })); err != nil {
		t.Fatal(err)
	}
	for _, label := range labels {
		for _, crash := range []bool{false, true} {
			t.Run(label+map[bool]string{false: "/error", true: "/exit"}[crash], func(t *testing.T) {
				data, next, old, _ := journalFixture(t)
				journalCrash(t, "commit", "install:metrics.db:rename", data, next)
				if crash {
					journalCrash(t, "recover", label, data, next)
				} else {
					err := recoverRestore(data, journalHook(osFSOps(), func(at string) error {
						if at == label {
							return errors.New("injected")
						}
						return nil
					}))
					var safe *Error
					if !errors.As(err, &safe) || safe.Code != "restore_recovery_failed" {
						t.Fatalf("fault not propagated: %v", err)
					}
				}
				if err := RecoverRestore(data); err != nil {
					t.Fatal(err)
				}
				journalAssertFiles(t, data, old)
			})
		}
	}
}

func TestRestoreJournalCommittedNeverReadsOld(t *testing.T) {
	data, next, _, fresh := journalFixture(t)
	journalCrash(t, "commit", "committed:durable", data, next)
	raw, err := os.ReadFile(filepath.Join(data, restoreJournalName))
	if err != nil {
		t.Fatal(err)
	}
	var journal restoreJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, ".restore-"+journal.ID, "old", "secrets.key")); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(data); err != nil {
		t.Fatal(err)
	}
	journalAssertFiles(t, data, fresh)
}

func TestRestoreJournalCanceledBeforePreparedPublication(t *testing.T) {
	data, next, old, _ := journalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := commitRestore(ctx, data, next, journalHook(osFSOps(), func(label string) error {
		if label == "prepared:file-sync" {
			cancel()
		}
		return nil
	}))
	if err == nil {
		t.Fatal("canceled preparation installed live data")
	}
	journalAssertFiles(t, data, old)
	if _, err := os.Stat(filepath.Join(data, restoreJournalName)); !os.IsNotExist(err) {
		t.Fatalf("canceled preparation published journal: %v", err)
	}
}

func TestRestoreJournalRecoveryAbsentAndCommittedOperations(t *testing.T) {
	for _, committed := range []bool{false, true} {
		fixture := func(t *testing.T) (string, string, map[string][]byte) {
			data, next, old, fresh := journalFixture(t)
			if committed {
				journalCrash(t, "commit", "committed:durable", data, next)
				return data, next, fresh
			}
			for _, name := range []string{"orbitbeacon.db-wal", "metrics.db", "secrets.key"} {
				if err := os.Remove(filepath.Join(data, name)); err != nil {
					t.Fatal(err)
				}
				delete(old, name)
			}
			journalCrash(t, "commit", "install:metrics.db:rename", data, next)
			return data, next, old
		}
		data, _, _ := fixture(t)
		var labels []string
		if err := recoverRestore(data, journalHook(osFSOps(), func(label string) error { labels = append(labels, label); return nil })); err != nil {
			t.Fatal(err)
		}
		for _, label := range labels {
			for _, crash := range []bool{false, true} {
				t.Run(map[bool]string{false: "absent/", true: "committed/"}[committed]+label+map[bool]string{false: "/error", true: "/exit"}[crash], func(t *testing.T) {
					data, next, want := fixture(t)
					if crash {
						journalCrash(t, "recover", label, data, next)
					} else {
						err := recoverRestore(data, journalHook(osFSOps(), func(at string) error {
							if at == label {
								return errors.New("injected")
							}
							return nil
						}))
						var safe *Error
						if !errors.As(err, &safe) || safe.Code != "restore_recovery_failed" {
							t.Fatalf("fault not propagated: %v", err)
						}
					}
					if err := RecoverRestore(data); err != nil {
						t.Fatal(err)
					}
					journalAssertFiles(t, data, want)
				})
			}
		}
	}
}

func TestRestoreJournalRejectsCorruptionWithoutMutation(t *testing.T) {
	for _, damage := range []string{"missing_old", "truncated_old", "symlink_old", "malformed", "unknown_field", "unknown_file", "bad_id", "duplicate_field"} {
		t.Run(damage, func(t *testing.T) {
			data, next, _, _ := journalFixture(t)
			journalCrash(t, "commit", "install:metrics.db:rename", data, next)
			path := filepath.Join(data, restoreJournalName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var j restoreJournal
			if err := json.Unmarshal(raw, &j); err != nil {
				t.Fatal(err)
			}
			oldPath := filepath.Join(data, ".restore-"+j.ID, "old", "orbitbeacon.db")
			switch damage {
			case "missing_old":
				err = os.Remove(oldPath)
			case "truncated_old":
				err = os.WriteFile(oldPath, []byte("bad"), 0600)
			case "symlink_old":
				if err = os.Remove(oldPath); err == nil {
					err = os.Symlink(filepath.Join(next, "orbitbeacon.db"), oldPath)
				}
			case "malformed":
				raw = []byte("{")
			case "unknown_field":
				raw = append([]byte(`{"extra":0,`), raw[1:]...)
			case "duplicate_field":
				raw = append([]byte(`{"version":1,`), raw[1:]...)
			case "unknown_file":
				raw = bytes.ReplaceAll(raw, []byte("orbitbeacon.db"), []byte("../escape.db"))
			case "bad_id":
				raw = bytes.ReplaceAll(raw, []byte(j.ID), []byte("../escape"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, name := range restoreFiles {
				if value, err := os.ReadFile(filepath.Join(data, name)); err == nil {
					before[name] = value
				}
			}
			err = RecoverRestore(data)
			var safe *Error
			if !errors.As(err, &safe) || safe.Code != "restore_recovery_failed" {
				t.Fatalf("corrupt journal accepted: %v", err)
			}
			journalAssertFiles(t, data, before)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatal("journal modified after corruption")
			}
		})
	}
}
