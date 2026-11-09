package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestUpdatePolicyTrustAndCapabilities(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native"}
	for _, raw := range []string{"http://release.example.test", "https://user@release.example.test", "https://release.example.test?a=1", "https://release.example.test?", "https://release.example.test#", "https://release.example.test#x"} {
		policy.UpdateBaseURL = raw
		if _, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, mode := range []string{"native", "docker", "foreground"} {
		for _, base := range []string{"", "https://release.example.test/download///"} {
			policy.InstallMode, policy.UpdateBaseURL = mode, base
			got, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy)
			if err != nil {
				t.Fatal(err)
			}
			if got.AllowsCommand("upgrade") != (mode == "native" && base != "") {
				t.Fatalf("mode=%s base=%s: %+v", mode, base, got)
			}
			if got.UpdateBaseURL != strings.TrimRight(base, "/") {
				t.Fatal(got.UpdateBaseURL)
			}
		}
	}
	policy.InstallMode, policy.UpdateBaseURL = "native", "https://release.example.test"
	for _, raw := range []string{policy.UpdateBaseURL, "https://attacker.example.test"} {
		opts := config.LoadOptions{File: configFile(t, map[string]any{"update_base_url": raw}), LookupEnv: lookup(nil)}
		if _, err := config.LoadAgent(opts, &policy); err == nil {
			t.Fatal("runtime update_base_url accepted")
		}
	}
}

func TestReadInstallPolicyAcceptsUpdateSource(t *testing.T) {
	path := filepath.Join(securePolicyDirectory(t), "install-policy.yaml")
	raw := "server_url: https://beacon.example.test\nupdate_base_url: https://release.example.test/download/\nstate_dir: /opt/orbit-beacon/state\ninstall_mode: native\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := config.ReadInstallPolicy(path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy)
	if err != nil || got.UpdateBaseURL != "https://release.example.test/download" || !got.AllowsCommand("upgrade") {
		t.Fatal(got, err)
	}
	report, err := json.Marshal(got.RuntimeSettings)
	if err != nil || strings.Contains(string(report), "update_base_url") || strings.Contains(string(report), "release.example.test") {
		t.Fatal(string(report), err)
	}
}
