package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func vmConfigFile(t *testing.T, values map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "orbitbeacon-f2-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "orbitbeacon-f2-config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVictoriaMetricsConfigurationPrecedence(t *testing.T) {
	file := vmConfigFile(t, map[string]any{"history_victoriametrics_url": "http://file.test/prefix/", "history_victoriametrics_token": "file-secret"})
	for _, tc := range []struct {
		name       string
		env, flags map[string]string
		url, token string
	}{
		{name: "file", url: "http://file.test/prefix", token: "file-secret"},
		{name: "env", env: map[string]string{"ORBITBEACON_HISTORY_VICTORIAMETRICS_URL": "https://env.test/vm///", "ORBITBEACON_HISTORY_VICTORIAMETRICS_TOKEN": "env-secret"}, url: "https://env.test/vm", token: "env-secret"},
		{name: "flag", env: map[string]string{"ORBITBEACON_HISTORY_VICTORIAMETRICS_URL": "http://env.test", "ORBITBEACON_HISTORY_VICTORIAMETRICS_TOKEN": "env-secret"}, flags: map[string]string{"history_victoriametrics_url": "https://flag.test/path/"}, url: "https://flag.test/path", token: "env-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.LoadServer(config.LoadOptions{File: file, LookupEnv: lookup(tc.env), Flags: tc.flags})
			if err != nil || cfg.HistoryVictoriaMetricsURL != tc.url || cfg.HistoryVictoriaMetricsToken != tc.token {
				t.Fatalf("configuration mismatch: err=%v", err)
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), tc.token) || strings.Contains(string(data), "history_victoriametrics_token") {
				t.Fatal("serialized token")
			}
		})
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(nil)})
	if err != nil || cfg.HistoryVictoriaMetricsURL != "" || cfg.HistoryVictoriaMetricsToken != "" {
		t.Fatalf("defaults: err=%v", err)
	}
}

func TestVictoriaMetricsConfigurationValidation(t *testing.T) {
	for _, raw := range []string{"ftp://vm.test", "vm.test", "http://", "http:///prefix", "http://user:secret@vm.test", "http://vm.test?x=secret", "http://vm.test?", "http://vm.test#secret", "http://vm.test#", "http://vm.test:bad", "http://vm.test:99999", "http://vm.test/%zz"} {
		for _, source := range []string{"file", "env", "flag"} {
			t.Run(source+"/"+raw, func(t *testing.T) {
				opts := config.LoadOptions{LookupEnv: lookup(nil)}
				switch source {
				case "file":
					opts.File = vmConfigFile(t, map[string]any{"history_victoriametrics_url": raw})
				case "env":
					opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_HISTORY_VICTORIAMETRICS_URL": raw})
				case "flag":
					opts.Flags = map[string]string{"history_victoriametrics_url": raw}
				}
				_, err := config.LoadServer(opts)
				if err == nil || !strings.Contains(err.Error(), "history_victoriametrics_url") || strings.Contains(err.Error(), "secret") {
					t.Fatalf("error=%v", err)
				}
			})
		}
	}
	for _, raw := range []string{"http://vm.test", "https://vm.test/prefix/", "http://127.0.0.1:8428/", "http://[::1]:8428/prefix"} {
		if _, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(nil), Flags: map[string]string{"history_victoriametrics_url": raw}}); err != nil {
			t.Errorf("URL=%s err=%v", raw, err)
		}
	}
	for _, source := range []string{"file", "env"} {
		t.Run("token without URL/"+source, func(t *testing.T) {
			opts := config.LoadOptions{LookupEnv: lookup(nil)}
			if source == "file" {
				opts.File = vmConfigFile(t, map[string]any{"history_victoriametrics_token": "private-token"})
			} else {
				opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_HISTORY_VICTORIAMETRICS_TOKEN": "private-token"})
			}
			_, err := config.LoadServer(opts)
			if err == nil || !strings.Contains(err.Error(), "history_victoriametrics_token") || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	_, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(nil), Flags: map[string]string{"history_victoriametrics_url": "http://vm.test", "history_victoriametrics_token": "private-token"}})
	if err == nil || !strings.Contains(err.Error(), "history_victoriametrics_token") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("token flag error=%v", err)
	}
}
