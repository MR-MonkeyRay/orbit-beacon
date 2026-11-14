package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestAgentLoaderReloadKeepsStartupOverridesAndPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	put := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "foreground", UpdateBaseURL: "https://releases.example.test"}
	t.Setenv("ORBITBEACON_REPORT_INTERVAL_SECONDS", "15")
	t.Setenv("ORBITBEACON_INTERFACES", `["eth0"]`)
	put("report_interval_seconds: 20\ndisk_interval_seconds: 120\nprobe_expected_status:\n  https://example.test/a,b: 204\n")
	flags := map[string]string{"disk_interval_seconds": "180", "probe_targets": `["https://example.test/a,b"]`}
	load, err := agentLoader(path, flags, policy)
	if err != nil {
		t.Fatal(err)
	}
	first, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if first.ReportIntervalSeconds != 15 || first.DiskIntervalSeconds != 180 || first.ProbeTargets[0] != "https://example.test/a,b" || first.ProbeExpectedStatus["https://example.test/a,b"] != 204 {
		t.Fatalf("startup = %+v", first)
	}
	t.Setenv("ORBITBEACON_REPORT_INTERVAL_SECONDS", "60")
	t.Setenv("ORBITBEACON_SERVER_URL", "https://untrusted.example.test")
	flags["disk_interval_seconds"] = "3600"
	policy.ServerURL = "https://changed.example.test"
	policy.UpdateBaseURL = "https://changed.example.test"
	t.Setenv("ORBITBEACON_UPDATE_CHANNEL", "disabled")
	t.Setenv("ORBITBEACON_UPDATE_BASE_URL", "https://untrusted.example.test")
	put("report_interval_seconds: 5\ndisk_interval_seconds: 60\ndisk_paths:\n  - /srv/a,b\nprobe_expected_status:\n  https://example.test/a,b: 200\n")
	next, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if next.ReportIntervalSeconds != 15 || next.DiskIntervalSeconds != 180 || next.ServerURL != first.ServerURL || next.Interfaces[0] != "eth0" || next.DiskPaths[0] != "/srv/a,b" || next.ProbeExpectedStatus["https://example.test/a,b"] != 200 {
		t.Fatalf("reload changed frozen sources: %+v", next)
	}
	if next.UpdateBaseURL != first.UpdateBaseURL || !next.AllowsCommand("check_update") {
		t.Fatal("reload changed trusted update source", next.InstallPolicy)
	}
	for _, field := range []string{"update_channel: disabled\n", "update_base_url: https://untrusted.example.test\n"} {
		put(field)
		if _, err := load(); err == nil {
			t.Fatal("reload accepted trusted source field", field)
		}
	}
	put("server_url: https://untrusted.example.test\n")
	if _, err := load(); err == nil {
		t.Fatal("reload changed protected policy")
	}
	put("interfaces: null\n")
	if _, err := load(); err == nil {
		t.Fatal("reload accepted null hidden by captured env override")
	}
	put("unknown: 1\n")
	if _, err := load(); err == nil {
		t.Fatal("reload accepted unknown local key")
	}
	put("disk_interval_seconds: [\n")
	if _, err := load(); err == nil {
		t.Fatal("reload accepted malformed YAML")
	}
}

func TestRuntimeFlagsAreAcceptedByAllRuntimeCommands(t *testing.T) {
	for _, command := range []string{"run", "enroll", "doctor"} {
		err := run([]string{command, "--report-interval-seconds", "5", "--disk-interval-seconds", "60", "--interfaces", `["eth0"]`, "--disk-paths", `["/srv/a,b"]`, "--probe-targets", `["https://example.test/a,b"]`, "--probe-expected-status", `{"https://example.test/a,b":204}`})
		if err == nil || !strings.Contains(err.Error(), "--install-policy") {
			t.Fatalf("%s runtime flags not parsed: %v", command, err)
		}
	}
	if err := run([]string{"update", "--version", "1.0.0"}); err == nil {
		t.Fatalf("removed update command = %v", err)
	}
}

func TestAgentLoaderWithoutFileSupportsStableReload(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "http://beacon.example.test:8080", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "foreground"}
	load, err := agentLoader("", nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	first, err := load()
	if err != nil {
		t.Fatal(err)
	}
	next, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if first.ServerURL != policy.ServerURL || next.ServerURL != policy.ServerURL || first.ReportIntervalSeconds != 10 || next.ReportIntervalSeconds != first.ReportIntervalSeconds || next.DiskIntervalSeconds != first.DiskIntervalSeconds {
		t.Fatalf("fileless reload changed defaults: %+v / %+v", first, next)
	}
}
