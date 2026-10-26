package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"orbitbeacon/internal/agent/collect"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func desired(revision int64, interval int) v1.DesiredConfig {
	return v1.DesiredConfig{Revision: revision, ReportIntervalSeconds: interval, DiskIntervalSeconds: 60, Interfaces: []string{"eth0"}, DiskPaths: []string{"/"}, ProbeTargets: []string{}, BillingDay: 1, QuotaBytes: "0"}
}

func TestDesiredConfigValidatesPersistsThenAppliesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	store, err := service.OpenConfigStore(context.Background(), path, desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oldFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	newConfig := desired(2, 30)
	if err := store.Apply(context.Background(), newConfig); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.Current(), newConfig) {
		t.Fatal("new config not applied")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk v1.DesiredConfig
	if err := json.Unmarshal(data, &disk); err != nil || !reflect.DeepEqual(disk, newConfig) {
		t.Fatalf("applied config not durable: %+v %v", disk, err)
	}
	oldContents, err := io.ReadAll(oldFile)
	if err != nil || !bytes.Equal(oldContents, oldBytes) {
		t.Fatal("config overwritten in place instead of atomic replacement")
	}
	reopened, err := service.OpenConfigStore(context.Background(), path, desired(1, 10))
	if err != nil || !reflect.DeepEqual(reopened.Current(), newConfig) {
		t.Fatalf("saved config not loaded: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("config not permission-safe: %v %v", info, err)
		}
	}
}

func TestRejectedConfigKeepsPriorDiskAndRuntimeRevision(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*v1.DesiredConfig)
	}{
		{"too frequent", func(c *v1.DesiredConfig) { c.ReportIntervalSeconds = 4 }},
		{"too slow", func(c *v1.DesiredConfig) { c.ReportIntervalSeconds = 61 }},
		{"disk frequency", func(c *v1.DesiredConfig) { c.DiskIntervalSeconds = 59 }},
		{"disk slow", func(c *v1.DesiredConfig) { c.DiskIntervalSeconds = 3601 }},
		{"billing zero", func(c *v1.DesiredConfig) { c.BillingDay = 0 }},
		{"billing high", func(c *v1.DesiredConfig) { c.BillingDay = 29 }},
		{"negative quota", func(c *v1.DesiredConfig) { c.QuotaBytes = "-1" }},
		{"too many disks", func(c *v1.DesiredConfig) {
			c.DiskPaths = []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g", "/h", "/i"}
		}},
		{"too many interfaces", func(c *v1.DesiredConfig) { c.Interfaces = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} }},
		{"relative disk", func(c *v1.DesiredConfig) { c.DiskPaths = []string{"relative/data"} }},
		{"drive relative disk", func(c *v1.DesiredConfig) { c.DiskPaths = []string{"C:relative"} }},
		{"oversized disk", func(c *v1.DesiredConfig) { c.DiskPaths = []string{"/" + strings.Repeat("a", 1024)} }},
		{"aggregate report expansion", func(c *v1.DesiredConfig) {
			c.DiskPaths = make([]string, 4)
			for i := range c.DiskPaths {
				c.DiskPaths[i] = "/" + strings.Repeat("<", 1000) + string(rune('a'+i))
			}
		}},
		{"duplicate disk", func(c *v1.DesiredConfig) { c.DiskPaths = []string{"/", "/"} }},
		{"NUL disk", func(c *v1.DesiredConfig) { c.DiskPaths = []string{"/data\x00suffix"} }},
		{"duplicate interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{"eth0", "eth0"} }},
		{"oversized interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{strings.Repeat("a", 256)} }},
		{"CR interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{"eth0\rheader"} }},
		{"LF interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{"eth0\nheader"} }},
		{"NUL interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{"eth0\x00suffix"} }},
		{"old revision", func(c *v1.DesiredConfig) { c.Revision = 1 }},
		{"same revision conflict", func(c *v1.DesiredConfig) { c.Revision = 2; c.ReportIntervalSeconds = 60 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "desired.json")
			prior := desired(2, 30)
			store, err := service.OpenConfigStore(context.Background(), path, prior)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			invalid := desired(3, 10)
			tt.mutate(&invalid)
			if err := store.Apply(context.Background(), invalid); err == nil {
				t.Fatal("invalid desired config accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) || !reflect.DeepEqual(store.Current(), prior) {
				t.Fatal("invalid config changed durable/runtime state")
			}
			failure := store.ConfigError()
			if failure == nil || failure.Revision != invalid.Revision || failure.Code != "invalid_config" {
				t.Fatalf("rejected revision not reportable: %+v", failure)
			}
		})
	}
}

func TestConfigPersistenceFailureDoesNotApplyInMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "desired.json")
	prior := desired(1, 10)
	store, err := service.OpenConfigStore(context.Background(), path, prior)
	if err != nil {
		t.Fatal(err)
	}
	// Replacing the destination with a directory makes atomic rename fail even as root.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(context.Background(), desired(2, 30)); err == nil {
		t.Fatal("failed durable write treated as applied")
	}
	if !reflect.DeepEqual(store.Current(), prior) {
		t.Fatal("runtime changed before successful durable replacement")
	}
	if store.ConfigError() == nil {
		t.Fatal("persistence failure not exposed in config_error")
	}
}

func TestDesiredConfigExactRevisionRetryIsIdempotentAndClearsEarlierError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	store, err := service.OpenConfigStore(context.Background(), path, desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	invalid := desired(2, 1)
	if err := store.Apply(context.Background(), invalid); err == nil {
		t.Fatal("invalid config accepted")
	}
	valid := desired(3, 30)
	if err := store.Apply(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	if store.ConfigError() != nil {
		t.Fatal("successful application retained stale config_error")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(context.Background(), valid); err != nil {
		t.Fatalf("exact repeated config rejected: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(store.Current(), valid) {
		t.Fatal("identical revision retry changed configuration")
	}
}

func TestEveryPersistedDesiredConfigIsCollectorCompatible(t *testing.T) {
	for _, tt := range []struct {
		name           string
		mutate         func(*v1.DesiredConfig)
		allowRejection bool
	}{
		{"ordinary", func(*v1.DesiredConfig) {}, false},
		{"automatic selection", func(c *v1.DesiredConfig) { c.DiskPaths = []string{}; c.Interfaces = []string{} }, false},
		{"bounded interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{strings.Repeat("a", 255)} }, false},
		{"relative disk", func(c *v1.DesiredConfig) { c.DiskPaths = []string{"data"} }, true},
		{"long interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{strings.Repeat("a", 256)} }, true},
		{"control interface", func(c *v1.DesiredConfig) { c.Interfaces = []string{"eth0\n"} }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := service.OpenConfigStore(context.Background(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 10))
			if err != nil {
				t.Fatal(err)
			}
			collector, err := collect.New(collect.Options{Config: store.Current()})
			if err != nil {
				t.Fatal(err)
			}
			next := desired(2, 30)
			tt.mutate(&next)
			if err := store.Apply(context.Background(), next); err != nil {
				if !tt.allowRejection {
					t.Fatalf("valid collector config rejected: %v", err)
				}
				return
			}
			if err := collector.SetConfig(store.Current()); err != nil {
				t.Fatalf("persisted revision %d cannot be collected: %v", store.Current().Revision, err)
			}
		})
	}
}

func TestLegacyOversizedConfigRetainsDiskStateUntilRemoteRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	legacy := desired(7, 10)
	legacy.DiskPaths = make([]string, 4)
	for i := range legacy.DiskPaths {
		legacy.DiskPaths[i] = "/" + strings.Repeat("<", 1000) + string(rune('a'+i))
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := service.OpenConfigStore(context.Background(), path, desired(1, 10))
	if err != nil {
		t.Fatalf("legacy selection prevents remote repair: %v", err)
	}
	effective := store.Current()
	if effective.Revision != 7 || len(effective.DiskPaths) != 0 || len(effective.Interfaces) != 0 {
		t.Fatalf("legacy diagnostic configuration = %+v", effective)
	}
	if err := effective.Validate(); err != nil {
		t.Fatalf("diagnostic selection cannot report: %v", err)
	}
	beforeRepair, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, beforeRepair) {
		t.Fatal("legacy configuration overwritten without a remote repair")
	}
	if failure := store.ConfigError(); failure == nil || failure.Revision != 7 {
		t.Fatalf("legacy rejection was silently acknowledged: %+v", failure)
	}
	repair := desired(8, 5)
	if err := store.Apply(context.Background(), repair); err != nil {
		t.Fatalf("higher-revision remote repair rejected: %v", err)
	}
	reopened, err := service.OpenConfigStore(context.Background(), path, desired(1, 10))
	if err != nil || !reflect.DeepEqual(reopened.Current(), repair) || reopened.ConfigError() != nil {
		t.Fatalf("remote repair not durable: %v", err)
	}
}

func TestBudgetRecoveryDoesNotAcceptOtherCorruptSavedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	prior := desired(1, 10)
	store, err := service.OpenConfigStore(context.Background(), path, prior)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := desired(2, 4)
	for i := range 4 {
		corrupt.DiskPaths = append(corrupt.DiskPaths, "/"+strings.Repeat("<", 1000)+string(rune('a'+i)))
	}
	data, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(context.Background(), desired(3, 5)); err == nil {
		t.Fatal("other corruption was incorrectly treated as a legacy budget error")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) || !reflect.DeepEqual(store.Current(), prior) {
		t.Fatal("corrupt saved configuration was overwritten or applied")
	}
}
