package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/config"
)

func yamlConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func invalidYAMLDocuments() map[string]string {
	return map[string]string{
		"empty":                   "",
		"null":                    "null\n",
		"sequence":                "- private-value\n",
		"scalar":                  "private-value\n",
		"malformed":               "private-value: [\n",
		"duplicate":               "max_nodes: 100\nmax_nodes: 200\n",
		"nested duplicate":        "database:\n  primary:\n    driver: sqlite\n    driver: mysql\n",
		"multiple documents":      "{}\n---\n{}\n",
		"empty trailing document": "{}\n---\n",
		"trailing syntax":         "{}\nprivate-value: [\n",
		"integer key":             "1: private-value\n",
		"boolean key":             "true: private-value\n",
		"composite key":           "? [private-value]\n: value\n",
		"nested integer key":      "database:\n  primary:\n    1: private-value\n",
		"nested sequence key":     "database:\n  primary:\n    tls:\n      ? [private-value]\n      : value\n",
		"nonfinite float":         "max_nodes: .nan\n",
	}
}

func TestYAMLServerNestedDatabaseAndDefaults(t *testing.T) {
	file := yamlConfigFile(t, `# Startup configuration uses the JSON field names.
public_url: https://beacon.example.test
access_log: true
max_nodes: 123
database:
  primary:
    driver: mysql
    address: db.example.test:3306
    user: beacon
    password_file: /run/secrets/db-password
    tls:
      server_name: db.example.test
      ca_file: /etc/beacon/db-ca.pem
  standby:
    driver: sqlite
  failover:
    max_loss_window: 0s
`)
	got, err := config.LoadServer(config.LoadOptions{File: file, LookupEnv: lookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ListenAddr != "127.0.0.1:8080" || got.WebUpdateIntervalSeconds != 10 || got.MaxNodes != 123 || !got.AccessLog || got.Database.Primary.Driver != "mysql" || got.Database.Primary.PasswordFile != "/run/secrets/db-password" || got.Database.Primary.TLS == nil || got.Database.Primary.TLS.CAFile != "/etc/beacon/db-ca.pem" || got.Database.Standby == nil || got.Database.Standby.Driver != "sqlite" || got.Database.Failover.MaxLossWindow != 0 || got.Database.Failover.FailureTimeout != 30*time.Second {
		t.Fatalf("unexpected YAML configuration: %+v", got)
	}
	got, err = config.LoadServer(config.LoadOptions{File: file, LookupEnv: lookup(map[string]string{"ORBITBEACON_MAX_NODES": "234", "ORBITBEACON_DATABASE": `{}`}), Flags: map[string]string{"max_nodes": "345", "database": `{"primary":{"driver":"sqlite"}}`}})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxNodes != 345 || got.Database.Primary.Driver != "sqlite" || got.Database.Standby != nil || !got.AccessLog {
		t.Fatalf("source precedence changed: %+v", got)
	}
}

func TestYAMLAgentArraysMapsAndOffSwitches(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native"}
	file := yamlConfigFile(t, `report_interval_seconds: 15
interfaces:
  - eth0
disk_paths:
  - /srv/custom,volume
probe_targets:
  - https://example.test/health
probe_expected_status:
  https://example.test/health: 204
remote_commands: []
disabled_features:
  - disk
`)
	got, err := config.LoadAgent(config.LoadOptions{File: file, LookupEnv: lookup(nil)}, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReportIntervalSeconds != 15 || got.DiskIntervalSeconds != 60 || !reflect.DeepEqual(got.Interfaces, []string{"eth0"}) || !reflect.DeepEqual(got.DiskPaths, []string{"/srv/custom,volume"}) || got.ProbeExpectedStatus["https://example.test/health"] != 204 || got.RemoteCommands == nil || len(got.RemoteCommands) != 0 || !got.FeatureDisabled("disk") {
		t.Fatalf("unexpected YAML runtime: %+v", got)
	}
}

func TestYAMLStartupRejectsInvalidStructureWithoutValues(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native"}
	for name, body := range invalidYAMLDocuments() {
		t.Run(name, func(t *testing.T) {
			opts := config.LoadOptions{File: yamlConfigFile(t, body), LookupEnv: lookup(nil)}
			_, serverErr := config.LoadServer(opts)
			_, agentErr := config.LoadAgent(opts, &policy)
			for _, err := range []error{serverErr, agentErr} {
				if err == nil || strings.Contains(err.Error(), "private-value") {
					t.Fatalf("invalid YAML must fail without revealing values: %v", err)
				}
			}
		})
	}
}

func TestYAMLScalarTypesRemainStrict(t *testing.T) {
	for _, body := range []string{"max_nodes: \"100\"\n", "max_nodes: 100.0\n", "access_log: \"true\"\n", "access_log: 1\n", "public_url: true\n", "database:\n  primary:\n    driver: mysql\n    address: db.test:3306\n    user: true\n    password_file: /run/password\n"} {
		if _, err := config.LoadServer(config.LoadOptions{File: yamlConfigFile(t, body), LookupEnv: lookup(nil)}); err == nil {
			t.Fatalf("accepted incorrect YAML scalar type: %s", body)
		}
	}
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, InstallMode: "native"}
	for _, body := range []string{"report_interval_seconds: \"10\"\n", "report_interval_seconds: 10.0\n", "expected_uid: \"1000\"\n", "expected_uid: 1000.0\n", "server_url: https://untrusted.example.test\n"} {
		_, err := config.LoadAgent(config.LoadOptions{File: yamlConfigFile(t, body), LookupEnv: lookup(nil), Flags: map[string]string{"report_interval_seconds": "10", "expected_uid": "1000", "server_url": policy.ServerURL}}, &policy)
		if err == nil {
			t.Fatalf("higher-priority values hid invalid YAML: %s", body)
		}
	}
}

