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
	for _, test := range []struct {
		name string
		base string
		want string
	}{
		{"default", "", "https://github.com/mr-monkeyray/orbit-beacon/releases/download"},
		{"mirror", "https://release.example.test/download///", "https://release.example.test/download"},
		{"official", "https://github.com/mr-monkeyray/orbit-beacon/releases/download", "https://github.com/mr-monkeyray/orbit-beacon/releases/download"},
		{"other GitHub repository", "https://github.com/another/project/releases/download///", "https://github.com/another/project/releases/download"},
		{"preserve repository case", "https://GitHub.com/MR-MonkeyRay/Orbit-Beacon/releases/download///", "https://github.com/MR-MonkeyRay/Orbit-Beacon/releases/download"},
		{"arbitrary HTTPS root", "https://github.com/mr-monkeyray/orbit-beacon/releases/download/other", "https://github.com/mr-monkeyray/orbit-beacon/releases/download/other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, mode := range []string{"native", "docker", "foreground"} {
				policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: mode, UpdateBaseURL: test.base}
				got, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy)
				if err != nil {
					t.Fatal(err)
				}
				if got.UpdateBaseURL != test.want || policy.UpdateBaseURL != test.base {
					t.Fatalf("normalized=%+v original=%+v", got.InstallPolicy, policy)
				}
				if !got.AllowsCommand("check_update") || got.AllowsCommand("upgrade") != (mode == "native") || got.AllowsCommand("restart") != (mode != "foreground") || got.AllowsCommand("stop") != (mode != "docker") {
					t.Fatalf("mode=%s commands=%v", mode, got.RemoteCommands)
				}
				for _, allowed := range [][]string{{}, {"restart", "stop"}, {"check_update"}, {"upgrade"}} {
					limited, err := config.LoadAgent(config.LoadOptions{File: configFile(t, map[string]any{"remote_commands": allowed}), LookupEnv: lookup(nil)}, &policy)
					if err != nil {
						t.Fatal(err)
					}
					for _, command := range []string{"check_update", "upgrade", "restart", "stop"} {
						want := got.AllowsCommand(command) && (config.RuntimeSettings{RemoteCommands: allowed}).AllowsCommand(command)
						if limited.AllowsCommand(command) != want {
							t.Fatalf("mode=%s local=%v effective=%v", mode, allowed, limited.RemoteCommands)
						}
					}
					if limited.UpdateBaseURL != test.want {
						t.Fatal("permissions changed trusted source", limited.InstallPolicy)
					}
				}
			}
		})
	}
}

func TestUpdatePolicyRejectsMalformedSources(t *testing.T) {
	for _, base := range []string{
		"http://release.example.test", "https://user@release.example.test", "https://user:secret@release.example.test",
		"https://release.example.test?a=1", "https://release.example.test?", "https://release.example.test#", "https://release.example.test#x",
		"https://", "//release.example.test", "release.example.test", "https://release.example.test:bad", "https://release.example.test:65536",
		"https://release.example.test/%zz", " https://release.example.test", "https://release.example.test/\n", " ",
	} {
		t.Run(base, func(t *testing.T) {
			policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native", UpdateBaseURL: base}
			if _, err := config.NormalizeUpdatePolicy(policy); err == nil {
				t.Fatal("normalizer accepted malformed source")
			}
			if _, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy); err == nil {
				t.Fatal("loader accepted malformed source")
			}
		})
	}
}

func TestUpdatePolicyCannotBeRuntimeConfigured(t *testing.T) {
	policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: t.TempDir(), InstallMode: "native"}
	for key, value := range map[string]string{"update_channel": "disabled", "update_base_url": "https://attacker.example.test"} {
		for _, opts := range []config.LoadOptions{
			{File: configFile(t, map[string]any{key: value}), LookupEnv: lookup(nil)},
			{Flags: map[string]string{key: value}, LookupEnv: lookup(nil)},
		} {
			if _, err := config.LoadAgent(opts, &policy); err == nil {
				t.Fatalf("runtime %s accepted", key)
			}
		}
	}
	got, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(map[string]string{"ORBITBEACON_UPDATE_CHANNEL": "custom", "ORBITBEACON_UPDATE_BASE_URL": "https://attacker.example.test"})}, &policy)
	if err != nil || got.UpdateBaseURL != "https://github.com/mr-monkeyray/orbit-beacon/releases/download" {
		t.Fatal("environment changed trusted update policy", got, err)
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
	if err != nil || got.UpdateBaseURL != "https://release.example.test/download" || policy.UpdateBaseURL != got.UpdateBaseURL || !got.AllowsCommand("upgrade") {
		t.Fatal(got, err)
	}
	report, err := json.Marshal(got.RuntimeSettings)
	if err != nil || strings.Contains(string(report), "update_channel") || strings.Contains(string(report), "update_base_url") || strings.Contains(string(report), "release.example.test") {
		t.Fatal(string(report), err)
	}
}

func TestReadInstallPolicyDefaultsAndNormalizesSource(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		base string
	}{
		{"omitted source defaults official", "", "https://github.com/mr-monkeyray/orbit-beacon/releases/download"},
		{"official source", "update_base_url: https://github.com/mr-monkeyray/orbit-beacon/releases/download///\n", "https://github.com/mr-monkeyray/orbit-beacon/releases/download"},
		{"mirror source", "update_base_url: https://mirror.example.test/download/\n", "https://mirror.example.test/download"},
		{"other GitHub repository", "update_base_url: https://github.com/another/project/releases/download/\n", "https://github.com/another/project/releases/download"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(securePolicyDirectory(t), "install-policy.yaml")
			raw := "server_url: https://beacon.example.test\nstate_dir: /opt/orbit-beacon/state\ninstall_mode: native\n" + test.raw
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			policy, err := config.ReadInstallPolicy(path, os.Geteuid())
			if err != nil {
				t.Fatal(err)
			}
			got, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy)
			if err != nil || policy.UpdateBaseURL != test.base || got.UpdateBaseURL != test.base || !got.AllowsCommand("check_update") || !got.AllowsCommand("upgrade") {
				t.Fatal(got, err)
			}
		})
	}
}
