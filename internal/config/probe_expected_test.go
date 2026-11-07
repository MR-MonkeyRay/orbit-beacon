package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestAgentProbeExpectedStatus(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native"}
	targets := `["https://example.test/health","http://example.test/","dns://example.test","icmp://example.test","tcp://example.test:443"]`
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{`{"https://example.test/health":204,"http://example.test/":100}`, true}, {`{}`, true},
		{`{"https://example.test/health":599}`, true}, {`{"https://missing.test":204}`, false},
		{`{"dns://example.test":204}`, false}, {`{"icmp://example.test":204}`, false}, {`{"tcp://example.test:443":204}`, false},
		{`{"https://example.test/health":99}`, false}, {`{"https://example.test/health":600}`, false},
		{`{"https://example.test/health":204.5}`, false}, {`{"https://example.test/health":"204"}`, false},
		{`{"https://example.test/health":null}`, false}, {`[]`, false}, {`null`, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			for _, source := range []string{"file", "env", "flag"} {
				opts := config.LoadOptions{LookupEnv: lookup(nil), Flags: map[string]string{"probe_targets": targets}}
				switch source {
				case "file":
					opts.File = configFile(t, map[string]any{"probe_expected_status": json.RawMessage(tc.value)})
				case "env":
					opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_PROBE_EXPECTED_STATUS": tc.value})
				case "flag":
					opts.Flags["probe_expected_status"] = tc.value
				}
				got, err := config.LoadAgent(opts, &policy)
				if (err == nil) != tc.valid {
					t.Fatalf("source=%s valid=%t: %v", source, tc.valid, err)
				}
				if err == nil {
					body, _ := json.Marshal(v1.Report{Runtime: got.RuntimeSettings})
					if strings.Contains(string(body), "probe_expected_status") {
						t.Fatalf("local setting leaked: %s", body)
					}
					var want map[string]int
					_ = json.Unmarshal([]byte(tc.value), &want)
					for key, value := range want {
						if got.ProbeExpectedStatus[key] != value {
							t.Fatalf("map=%v", got.ProbeExpectedStatus)
						}
					}
				}
			}
		})
	}
}

func TestAgentProbeExpectedStatusRejectsHiddenInvalidSource(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native"}
	for _, hidden := range []map[string]any{
		{"https://example.test": 204.5}, {"https://example.test": 99}, {"https://example.test": 600},
		{"dns://example.test": 204}, {"ftp://example.test": 204},
	} {
		_, err := config.LoadAgent(config.LoadOptions{File: configFile(t, map[string]any{"probe_expected_status": hidden}), LookupEnv: lookup(nil), Flags: map[string]string{"probe_expected_status": "{}"}}, &policy)
		if err == nil {
			t.Fatalf("valid override hid invalid source %v", hidden)
		}
	}
}

func TestAgentProbeExpectedStatusClearedWithProbesDisabled(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native"}
	got, err := config.LoadAgent(config.LoadOptions{File: configFile(t, map[string]any{
		"probe_targets":         []string{"https://example.test"},
		"probe_expected_status": map[string]int{"https://example.test": 204},
		"disabled_features":     []string{v1.FeatureProbes},
	}), LookupEnv: lookup(nil)}, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := v1.ValidateProbeExpectedStatus(got.ProbeTargets, got.ProbeExpectedStatus); err != nil {
		t.Fatalf("disabled probes left a status the runtime rejects: %v", err)
	}
}
