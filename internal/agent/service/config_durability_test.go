package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
)

func durabilityConfig(revision int64) v1.DesiredConfig {
	return v1.DesiredConfig{Revision: revision, ReportIntervalSeconds: 10, DiskIntervalSeconds: 60, Interfaces: []string{"eth0"}, DiskPaths: []string{"/"}, ProbeTargets: []string{}, BillingDay: 1, QuotaBytes: "0"}
}

func readDurabilityConfig(t *testing.T, path string) (v1.DesiredConfig, []byte, os.FileInfo) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved v1.DesiredConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return saved, data, info
}

func TestConfigStoreApplyRetryRequiresDirectorySync(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "desired.json")
	prior, next := durabilityConfig(1), durabilityConfig(2)
	store, err := OpenConfigStore(ctx, path, prior)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := OpenConfigStore(ctx, path, prior)
	if err != nil {
		t.Fatal(err)
	}
	realSync := store.syncDirectory
	failSync := true
	syncDirectory := func(dir string) error {
		if dir != filepath.Dir(path) {
			t.Fatalf("directory sync path = %q", dir)
		}
		saved, _, _ := readDurabilityConfig(t, path)
		if !reflect.DeepEqual(saved, next) {
			t.Fatalf("directory sync ran before replacement became visible: %+v", saved)
		}
		if failSync {
			return syscall.EIO
		}
		return realSync(dir)
	}
	store.syncDirectory = syncDirectory
	stale.syncDirectory = syncDirectory
	if err := store.Apply(ctx, next); !errors.Is(err, syscall.EIO) {
		t.Fatalf("post-rename sync failure = %v", err)
	}
	saved, contents, renamed := readDurabilityConfig(t, path)
	if !reflect.DeepEqual(saved, next) || !reflect.DeepEqual(store.Current(), prior) {
		t.Fatal("failed directory sync did not leave visible replacement and prior runtime state")
	}
	failure := store.ConfigError()
	if failure == nil || failure.Revision != next.Revision {
		t.Fatalf("failed application was acknowledged: %+v", failure)
	}
	for attempt := range 2 {
		if err := store.Apply(ctx, next); !errors.Is(err, syscall.EIO) {
			t.Fatalf("retry %d acknowledged an unsynced revision: %v", attempt, err)
		}
		if !reflect.DeepEqual(store.Current(), prior) || !reflect.DeepEqual(store.ConfigError(), failure) {
			t.Fatal("failed retry published config or cleared its error")
		}
		_, after, info := readDurabilityConfig(t, path)
		if !os.SameFile(renamed, info) || !bytes.Equal(contents, after) {
			t.Fatal("failed exact retry needlessly replaced the visible config")
		}
	}
	if err := stale.Apply(ctx, next); !errors.Is(err, syscall.EIO) {
		t.Fatalf("another store acknowledged the unsynced replacement: %v", err)
	}
	if !reflect.DeepEqual(stale.Current(), prior) || stale.ConfigError() == nil {
		t.Fatal("another store published the unsynced revision")
	}
	failSync = false
	if err := store.Apply(ctx, next); err != nil {
		t.Fatalf("retry after directory sync recovered: %v", err)
	}
	_, after, info := readDurabilityConfig(t, path)
	if !os.SameFile(renamed, info) || !bytes.Equal(contents, after) {
		t.Fatal("successful exact retry needlessly replaced the visible config")
	}
	if !reflect.DeepEqual(store.Current(), next) || store.ConfigError() != nil {
		t.Fatal("successful directory sync did not publish config and clear its error")
	}
	reopened, err := OpenConfigStore(ctx, path, prior)
	if err != nil || !reflect.DeepEqual(reopened.Current(), next) || reopened.ConfigError() != nil {
		t.Fatalf("reopened store did not load the recovered config: %v", err)
	}
}

func TestOpenConfigStoreExistingRequiresDirectorySync(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "valid"
		if legacy {
			name = "legacy_budget"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "desired.json")
			saved := durabilityConfig(7)
			if legacy {
				saved.DiskPaths = make([]string, 4)
				for i := range saved.DiskPaths {
					saved.DiskPaths[i] = "/" + strings.Repeat("<", 1000) + string(rune('a'+i))
				}
				if !errors.Is(saved.Validate(), v1.ErrReportBudget) {
					t.Fatal("legacy fixture does not exceed report budget")
				}
			}
			data, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, _, before := readDurabilityConfig(t, path)
			for attempt := range 2 {
				store, err := openConfigStore(ctx, path, durabilityConfig(1), func(dir string) error {
					if dir != filepath.Dir(path) {
						t.Fatalf("directory sync path = %q", dir)
					}
					actual, _, _ := readDurabilityConfig(t, path)
					if !reflect.DeepEqual(actual, saved) {
						t.Fatal("open did not preserve the visible saved config")
					}
					return syscall.EIO
				})
				if !errors.Is(err, syscall.EIO) || store != nil {
					t.Fatalf("open %d exposed config without directory sync: store=%v err=%v", attempt, store, err)
				}
			}
			store, err := OpenConfigStore(ctx, path, durabilityConfig(1))
			if err != nil {
				t.Fatal(err)
			}
			expected := cloneConfig(saved)
			if legacy {
				expected.DiskPaths = []string{}
				expected.Interfaces = []string{}
				if failure := store.ConfigError(); failure == nil || failure.Revision != saved.Revision || failure.Code != "report_size_exceeded" {
					t.Fatalf("legacy diagnostic error = %+v", failure)
				}
			} else if store.ConfigError() != nil {
				t.Fatal("valid saved config retained a failure")
			}
			if !reflect.DeepEqual(store.Current(), expected) {
				t.Fatalf("reopened config = %+v, want %+v", store.Current(), expected)
			}
			if legacy {
				if err := store.Apply(ctx, expected); err == nil {
					t.Fatal("same-revision replacement falsely acknowledged an unsaved budget repair")
				}
				if store.ConfigError() == nil || !reflect.DeepEqual(store.Current(), expected) {
					t.Fatal("rejected same-revision repair cleared its diagnostic or changed runtime state")
				}
			}
			_, after, info := readDurabilityConfig(t, path)
			if !os.SameFile(before, info) || !bytes.Equal(data, after) {
				t.Fatal("opening or rejecting a same-revision repair replaced the saved config")
			}
		})
	}
}

func TestConfigStoreExactRetryRestoresMissingFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "desired.json")
	prior := durabilityConfig(2)
	store, err := OpenConfigStore(ctx, path, prior)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	realSync := store.syncDirectory
	store.syncDirectory = func(string) error { return syscall.EIO }
	if err := store.Apply(ctx, prior); !errors.Is(err, syscall.EIO) {
		t.Fatalf("missing-file retry did not report directory sync failure: %v", err)
	}
	saved, contents, before := readDurabilityConfig(t, path)
	if !reflect.DeepEqual(saved, prior) || !reflect.DeepEqual(store.Current(), prior) || store.ConfigError() == nil {
		t.Fatal("missing-file retry did not restore the file while retaining the failed runtime state")
	}
	store.syncDirectory = realSync
	if err := store.Apply(ctx, prior); err != nil {
		t.Fatal(err)
	}
	_, after, info := readDurabilityConfig(t, path)
	if !os.SameFile(before, info) || !bytes.Equal(contents, after) || store.ConfigError() != nil {
		t.Fatal("recovered missing-file retry rewrote the file or retained its failure")
	}
}

func TestConfigStoreExactRetryRestoresCachedRevision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "desired.json")
	prior, latest := durabilityConfig(1), durabilityConfig(2)
	store, err := OpenConfigStore(ctx, path, prior)
	if err != nil {
		t.Fatal(err)
	}
	_, older, _ := readDurabilityConfig(t, path)
	if err := store.Apply(ctx, latest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, older, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(ctx, latest); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := readDurabilityConfig(t, path)
	if !reflect.DeepEqual(saved, latest) || !reflect.DeepEqual(store.Current(), latest) || store.ConfigError() != nil {
		t.Fatal("same-revision cache fallback acknowledged an older file instead of restoring it")
	}
}
