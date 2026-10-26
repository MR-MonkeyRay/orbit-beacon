package service_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestConfigStoreStaleApplyCannotRegressDurableRevision(t *testing.T) {
	for _, tt := range []struct {
		name string
		next v1.DesiredConfig
	}{
		{"older revision", desired(2, 20)},
		{"conflicting revision", desired(3, 20)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "desired.json")
			writer, err := service.OpenConfigStore(path, desired(1, 10))
			if err != nil {
				t.Fatal(err)
			}
			stale, err := service.OpenConfigStore(path, desired(1, 10))
			if err != nil {
				t.Fatal(err)
			}
			latest := desired(3, 30)
			if err := writer.Apply(latest); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := stale.Apply(tt.next); err == nil {
				t.Error("stale store accepted a revision rejected by durable state")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("stale store replaced the latest durable config")
			}
			failure := stale.ConfigError()
			if failure == nil || failure.Revision != tt.next.Revision || failure.Code != "invalid_config" {
				t.Errorf("stale rejection is not reportable: %+v", failure)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(executable, "-test.run=^TestConfigStoreRevisionSubprocess$")
			command.Env = append(os.Environ(), "ORBITBEACON_CONFIG_REOPEN_PATH="+path)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("new process did not load the highest revision: %v\n%s", err, output)
			}
		})
	}
}

func TestConfigStoreStaleExactRetryLoadsDurableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.json")
	writer, err := service.OpenConfigStore(path, desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := service.OpenConfigStore(path, desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	latest := desired(3, 30)
	if err := writer.Apply(latest); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Apply(latest); err != nil {
		t.Fatalf("identical durable revision rejected: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("identical durable revision was needlessly replaced")
	}
	if !reflect.DeepEqual(stale.Current(), latest) || stale.ConfigError() != nil {
		t.Error("identical retry did not refresh runtime config")
	}
}

func TestConfigStoreRevisionSubprocess(t *testing.T) {
	path := os.Getenv("ORBITBEACON_CONFIG_REOPEN_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	store, err := service.OpenConfigStore(path, desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Current(); !reflect.DeepEqual(got, desired(3, 30)) {
		t.Fatalf("durable config regressed: %+v", got)
	}
}
