package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestAgentLoaderReloadKeepsStartupOverridesAndPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	put := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "foreground"}
	t.Setenv("ORBITBEACON_REPORT_INTERVAL_SECONDS", "15")
	t.Setenv("ORBITBEACON_INTERFACES", `["eth0"]`)
	put(`{"report_interval_seconds":20,"disk_interval_seconds":120}`)
	flags := map[string]string{"disk_interval_seconds": "180", "probe_targets": `["https://example.test/a,b"]`}
	load, err := agentLoader(path, flags, policy)
	if err != nil {
		t.Fatal(err)
	}
	first, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if first.ReportIntervalSeconds != 15 || first.DiskIntervalSeconds != 180 || first.ProbeTargets[0] != "https://example.test/a,b" {
		t.Fatalf("startup = %+v", first)
	}
	t.Setenv("ORBITBEACON_REPORT_INTERVAL_SECONDS", "60")
	t.Setenv("ORBITBEACON_SERVER_URL", "https://untrusted.example.test")
	flags["disk_interval_seconds"] = "3600"
	policy.ServerURL = "https://changed.example.test"
	put(`{"report_interval_seconds":5,"disk_interval_seconds":60,"disk_paths":["/srv/a,b"]}`)
	next, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if next.ReportIntervalSeconds != 15 || next.DiskIntervalSeconds != 180 || next.ServerURL != first.ServerURL || next.Interfaces[0] != "eth0" || next.DiskPaths[0] != "/srv/a,b" {
		t.Fatalf("reload changed frozen sources: %+v", next)
	}
	put(`{"server_url":"https://untrusted.example.test"}`)
	if _, err := load(); err == nil {
		t.Fatal("reload changed protected policy")
	}
	put(`{"interfaces":null}`)
	if _, err := load(); err == nil {
		t.Fatal("reload accepted null hidden by captured env override")
	}
	put(`{"unknown":1}`)
	if _, err := load(); err == nil {
		t.Fatal("reload accepted unknown local key")
	}
	put(`{"disk_interval_seconds":`)
	if _, err := load(); err == nil {
		t.Fatal("reload accepted malformed JSON")
	}
}

func TestRuntimeFlagsAreAcceptedByAllRuntimeCommands(t *testing.T) {
	for _, command := range []string{"run", "enroll", "doctor"} {
		err := run([]string{command, "--report-interval-seconds", "5", "--disk-interval-seconds", "60", "--interfaces", `["eth0"]`, "--disk-paths", `["/srv/a,b"]`, "--probe-targets", `["https://example.test/a,b"]`})
		if err == nil || !strings.Contains(err.Error(), "--install-policy is required") {
			t.Fatalf("%s runtime flags not parsed: %v", command, err)
		}
	}
	if err := run([]string{"update", "--version", "1.0.0"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("removed update command = %v", err)
	}
}

func TestAgentLoaderWithoutFileSupportsStableReload(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "foreground"}
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
	if first.ReportIntervalSeconds != 10 || next.ReportIntervalSeconds != first.ReportIntervalSeconds || next.DiskIntervalSeconds != first.DiskIntervalSeconds {
		t.Fatalf("fileless reload changed defaults: %+v / %+v", first, next)
	}
}
