package config_test

import (
	"reflect"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestAgentLocalRuntimeSettingsPrecedence(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
	file := configFile(t, map[string]any{"report_interval_seconds": 20, "disk_interval_seconds": 120, "interfaces": []string{"eth0"}, "disk_paths": []string{"/srv/a,b"}, "probe_targets": []string{"https://example.test/a,b"}})
	got, err := config.LoadAgent(config.LoadOptions{File: file, LookupEnv: lookup(map[string]string{"ORBITBEACON_REPORT_INTERVAL_SECONDS": "15", "ORBITBEACON_INTERFACES": `["eth1"]`}), Flags: map[string]string{"report_interval_seconds": "5", "probe_targets": `[]`}}, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReportIntervalSeconds != 5 || got.DiskIntervalSeconds != 120 || !reflect.DeepEqual(got.Interfaces, []string{"eth1"}) || !reflect.DeepEqual(got.DiskPaths, []string{"/srv/a,b"}) || len(got.ProbeTargets) != 0 {
		t.Fatalf("resolved runtime = %+v", got)
	}
	defaults, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.ReportIntervalSeconds != 10 || defaults.DiskIntervalSeconds != 60 || defaults.Interfaces == nil || defaults.DiskPaths == nil || defaults.ProbeTargets == nil {
		t.Fatalf("defaults = %+v", defaults)
	}
}

func TestAgentLocalRuntimeRejectsInvalidConfiguration(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
	for _, value := range []any{nil, 1, "5", 5.5} {
		if _, err := config.LoadAgent(config.LoadOptions{File: configFile(t, map[string]any{"report_interval_seconds": value}), LookupEnv: lookup(nil)}, &policy); err == nil {
			t.Fatalf("accepted report interval %#v", value)
		}
	}
	for _, value := range []string{`null`, `eth0,eth1`, `[1]`, `["eth0","eth0"]`} {
		if _, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(map[string]string{"ORBITBEACON_INTERFACES": value})}, &policy); err == nil {
			t.Fatalf("accepted interface array %q", value)
		}
	}
	file := configFile(t, map[string]any{"server_url": "https://untrusted.example.test", "report_interval_seconds": 10})
	if _, err := config.LoadAgent(config.LoadOptions{File: file, LookupEnv: lookup(nil), Flags: map[string]string{"server_url": policy.ServerURL}}, &policy); err == nil {
		t.Fatal("runtime flag masked protected lower-layer conflict")
	}
}

func TestAgentRuntimeRejectsInvalidEverySourceAndHiddenLowerLayer(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
	for _, test := range []struct {
		key     string
		invalid any
		text    string
		valid   string
	}{
		{"report_interval_seconds", 4, "4", "10"},
		{"disk_interval_seconds", 59, "59", "60"},
		{"interfaces", []string{"eth0", "eth0"}, `["eth0","eth0"]`, `[]`},
		{"disk_paths", []string{"relative"}, `["relative"]`, `[]`},
		{"probe_targets", []string{"bad"}, `["bad"]`, `[]`},
	} {
		t.Run(test.key, func(t *testing.T) {
			for _, source := range []string{"JSON", "environment", "flag"} {
				opts := config.LoadOptions{LookupEnv: lookup(nil)}
				switch source {
				case "JSON":
					opts.File = configFile(t, map[string]any{test.key: test.invalid})
				case "environment":
					opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_" + strings.ToUpper(test.key): test.text})
				case "flag":
					opts.Flags = map[string]string{test.key: test.text}
				}
				if _, err := config.LoadAgent(opts, &policy); err == nil {
					t.Fatalf("accepted invalid %s from %s", test.key, source)
				}
			}
			for _, invalid := range []any{nil, test.invalid} {
				opts := config.LoadOptions{File: configFile(t, map[string]any{test.key: invalid}), LookupEnv: lookup(nil), Flags: map[string]string{test.key: test.valid}}
				if _, err := config.LoadAgent(opts, &policy); err == nil {
					t.Fatal("higher override hid invalid local JSON")
				}
			}
		})
	}
}

func TestAgentProtectedFieldsRejectCompositeValuesWithoutPanic(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
	for _, key := range []string{"server_url", "ca_file", "state_dir", "expected_uid", "expected_gid", "expected_account", "install_mode"} {
		for _, value := range []any{[]string{"unexpected"}, map[string]any{"unexpected": true}} {
			if _, err := config.LoadAgent(config.LoadOptions{File: configFile(t, map[string]any{key: value}), LookupEnv: lookup(nil)}, &policy); err == nil {
				t.Fatalf("accepted protected composite %s=%v", key, value)
			}
		}
	}
}
