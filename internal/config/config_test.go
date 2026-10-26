package config_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/config"
)

func configFile(t *testing.T, values map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestServerConfigurationPrecedence(t *testing.T) {
	file := configFile(t, map[string]any{
		"listen_addr": "127.0.0.1:8101",
		"data_dir":    t.TempDir(),
		"public_url":  "https://file.example.test",
		"max_nodes":   101,
	})
	for _, tt := range []struct {
		name         string
		file         string
		env          map[string]string
		flags        map[string]string
		wantAddress  string
		wantCapacity int
	}{
		{name: "defaults", wantAddress: "127.0.0.1:8080", wantCapacity: 500},
		{name: "JSON overrides defaults", file: file, wantAddress: "127.0.0.1:8101", wantCapacity: 101},
		{name: "environment overrides JSON", file: file, env: map[string]string{
			"ORBITBEACON_LISTEN_ADDR": "127.0.0.1:8102", "ORBITBEACON_MAX_NODES": "202",
		}, wantAddress: "127.0.0.1:8102", wantCapacity: 202},
		{name: "flags override environment", file: file, env: map[string]string{
			"ORBITBEACON_LISTEN_ADDR": "127.0.0.1:8102", "ORBITBEACON_MAX_NODES": "202",
		}, flags: map[string]string{"listen_addr": "127.0.0.1:8103", "max_nodes": "303"},
			wantAddress: "127.0.0.1:8103", wantCapacity: 303},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.LoadServer(config.LoadOptions{
				File: tt.file, LookupEnv: lookup(tt.env), Flags: tt.flags,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.ListenAddr != tt.wantAddress || got.MaxNodes != tt.wantCapacity {
				t.Errorf("configuration = (%q, %d), want (%q, %d)", got.ListenAddr, got.MaxNodes, tt.wantAddress, tt.wantCapacity)
			}
		})
	}
}

func TestServerConfigurationRejectsInvalidValuesFromEverySource(t *testing.T) {
	for _, tt := range []struct {
		name  string
		key   string
		env   string
		value string
	}{
		{"relative public URL", "public_url", "ORBITBEACON_PUBLIC_URL", "/dashboard"},
		{"HTTP public URL", "public_url", "ORBITBEACON_PUBLIC_URL", "http://example.test"},
		{"missing public host", "public_url", "ORBITBEACON_PUBLIC_URL", "https:///dashboard"},
		{"public URL credentials", "public_url", "ORBITBEACON_PUBLIC_URL", "https://user:password@example.test"},
		{"public URL query", "public_url", "ORBITBEACON_PUBLIC_URL", "https://example.test/?token=secret"},
		{"zero capacity", "max_nodes", "ORBITBEACON_MAX_NODES", "0"},
		{"negative capacity", "max_nodes", "ORBITBEACON_MAX_NODES", "-1"},
		{"noninteger capacity", "max_nodes", "ORBITBEACON_MAX_NODES", "1.5"},
		{"SQLite URI directory", "data_dir", "ORBITBEACON_DATA_DIR", "file:orbit.db?mode=memory&cache=shared"},
		{"network directory URI", "data_dir", "ORBITBEACON_DATA_DIR", "https://example.test/database"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range []string{"JSON", "environment", "flag"} {
				t.Run(source, func(t *testing.T) {
					values := map[string]any{"data_dir": t.TempDir(), "public_url": "https://beacon.example.test"}
					opts := config.LoadOptions{LookupEnv: lookup(nil)}
					switch source {
					case "JSON":
						var value any = tt.value
						if tt.key == "max_nodes" {
							if err := json.Unmarshal([]byte(tt.value), &value); err != nil {
								t.Fatal(err)
							}
						}
						values[tt.key] = value
					case "environment":
						opts.LookupEnv = lookup(map[string]string{tt.env: tt.value})
					case "flag":
						opts.Flags = map[string]string{tt.key: tt.value}
					}
					opts.File = configFile(t, values)
					if _, err := config.LoadServer(opts); err == nil {
						t.Fatalf("accepted invalid %s from %s", tt.key, source)
					}
				})
			}
		})
	}
}

func TestServerConfigurationAcceptsHTTPSAndExplicitCapacityIncrease(t *testing.T) {
	dir := t.TempDir()
	got, err := config.LoadServer(config.LoadOptions{
		File: configFile(t, map[string]any{
			"public_url": "https://beacon.example.test:8443",
			"data_dir":   dir, "max_nodes": 750,
		}), LookupEnv: lookup(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicURL != "https://beacon.example.test:8443" || got.DataDir != dir || got.MaxNodes != 750 {
		t.Errorf("valid configuration changed: %+v", got)
	}
}

func TestAgentInstallPolicyCannotBeOverridden(t *testing.T) {
	base := t.TempDir()
	policy := config.InstallPolicy{
		ServerURL:   "https://beacon.example.test",
		CAFile:      filepath.Join(base, "ca.pem"),
		StateDir:    filepath.Join(base, "state"),
		RuntimeDir:  filepath.Join(base, "runtime"),
		ExpectedUID: 1000, ExpectedGID: 1000,
		ExpectedAccount: "orbitbeacon",
		InstallMode:     "native", TrustKeyFile: filepath.Join(base, "release.pub"),
	}
	for _, tt := range []struct {
		key   string
		env   string
		value any
		text  string
	}{
		{"server_url", "ORBITBEACON_SERVER_URL", "https://attacker.example.test", "https://attacker.example.test"},
		{"ca_file", "ORBITBEACON_CA_FILE", "/other/ca.pem", "/other/ca.pem"},
		{"state_dir", "ORBITBEACON_STATE_DIR", "/other/state", "/other/state"},
		{"runtime_dir", "ORBITBEACON_RUNTIME_DIR", "/other/runtime", "/other/runtime"},
		{"expected_uid", "ORBITBEACON_EXPECTED_UID", 1001, "1001"},
		{"expected_gid", "ORBITBEACON_EXPECTED_GID", 1001, "1001"},
		{"expected_account", "ORBITBEACON_EXPECTED_ACCOUNT", "Administrator", "Administrator"},
		{"install_mode", "ORBITBEACON_INSTALL_MODE", "docker", "docker"},
		{"trust_key_file", "ORBITBEACON_TRUST_KEY_FILE", "/other/release.pub", "/other/release.pub"},
	} {
		t.Run(tt.key, func(t *testing.T) {
			for _, source := range []string{"JSON", "environment", "flag"} {
				t.Run(source, func(t *testing.T) {
					opts := config.LoadOptions{LookupEnv: lookup(nil)}
					switch source {
					case "JSON":
						opts.File = configFile(t, map[string]any{tt.key: tt.value})
					case "environment":
						opts.LookupEnv = lookup(map[string]string{tt.env: tt.text})
					case "flag":
						opts.Flags = map[string]string{tt.key: tt.text}
					}
					if _, err := config.LoadAgent(opts, &policy); err == nil {
						t.Fatalf("accepted conflicting protected %s from %s", tt.key, source)
					}
				})
			}
		})
	}
	if _, err := config.LoadAgent(config.LoadOptions{
		File:      configFile(t, map[string]any{"server_url": "https://attacker.example.test"}),
		LookupEnv: lookup(nil), Flags: map[string]string{"server_url": policy.ServerURL},
	}, &policy); err == nil {
		t.Error("higher-precedence matching flag concealed a conflicting protected file value")
	}
	got, err := config.LoadAgent(config.LoadOptions{
		LookupEnv: lookup(nil), Flags: map[string]string{"server_url": policy.ServerURL, "expected_uid": "1000"},
	}, &policy)
	if err != nil {
		t.Fatalf("matching protected values must be accepted: %v", err)
	}
	if got.ServerURL != policy.ServerURL || got.ExpectedUID != 1000 || got.TrustKeyFile != policy.TrustKeyFile {
		t.Errorf("policy was not retained: %+v", got)
	}
}

func TestServerValidatesOnlyResolvedOrdinaryConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name        string
		key         string
		env         string
		invalid     any
		invalidText string
		valid       any
		validText   string
	}{
		{"capacity", "max_nodes", "ORBITBEACON_MAX_NODES", 0, "0", 25, "25"},
		{"unparseable capacity", "max_nodes", "ORBITBEACON_MAX_NODES", "not-an-integer", "not-an-integer", 25, "25"},
		{"public URL", "public_url", "ORBITBEACON_PUBLIC_URL", "http://invalid.test", "http://invalid.test", "https://beacon.example.test", "https://beacon.example.test"},
		{"data directory", "data_dir", "ORBITBEACON_DATA_DIR", "file:unsafe.db", "file:unsafe.db", "/var/lib/orbitbeacon", "/var/lib/orbitbeacon"},
		{"listen address", "listen_addr", "ORBITBEACON_LISTEN_ADDR", "not-an-address", "not-an-address", "127.0.0.1:8181", "127.0.0.1:8181"},
		{"proxy CIDRs", "trusted_proxy_cidrs", "ORBITBEACON_TRUSTED_PROXY_CIDRS", []string{"invalid"}, "invalid", []string{"127.0.0.0/8"}, "127.0.0.0/8"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range []string{"environment overrides file", "flag overrides file", "flag overrides environment"} {
				t.Run(source, func(t *testing.T) {
					values := map[string]any{"data_dir": t.TempDir()}
					opts := config.LoadOptions{LookupEnv: lookup(nil)}
					switch source {
					case "environment overrides file":
						values[tt.key] = tt.invalid
						opts.LookupEnv = lookup(map[string]string{tt.env: tt.validText})
					case "flag overrides file":
						values[tt.key] = tt.invalid
						opts.Flags = map[string]string{tt.key: tt.validText}
					case "flag overrides environment":
						opts.LookupEnv = lookup(map[string]string{tt.env: tt.invalidText})
						opts.Flags = map[string]string{tt.key: tt.validText}
					}
					opts.File = configFile(t, values)
					got, err := config.LoadServer(opts)
					if err != nil {
						t.Fatalf("overridden ordinary value prevented loading: %v", err)
					}
					data, err := json.Marshal(got)
					if err != nil {
						t.Fatal(err)
					}
					var resolved map[string]json.RawMessage
					if err := json.Unmarshal(data, &resolved); err != nil {
						t.Fatal(err)
					}
					want, err := json.Marshal(tt.valid)
					if err != nil {
						t.Fatal(err)
					}
					if string(resolved[tt.key]) != string(want) {
						t.Errorf("resolved %s = %s, want %s", tt.key, resolved[tt.key], want)
					}
				})
			}
			// A valid lower layer must not conceal an invalid resolved value.
			if _, err := config.LoadServer(config.LoadOptions{
				File:      configFile(t, map[string]any{"data_dir": t.TempDir(), tt.key: tt.valid}),
				LookupEnv: lookup(map[string]string{tt.env: tt.validText}),
				Flags:     map[string]string{tt.key: tt.invalidText},
			}); err == nil {
				t.Error("accepted invalid highest-precedence value")
			}
		})
	}
}

func TestServerTLSPairCanBeComposedAcrossPrecedenceLayers(t *testing.T) {
	for _, tt := range []struct {
		name  string
		file  map[string]any
		env   map[string]string
		flags map[string]string
	}{
		{"file certificate and environment key", map[string]any{"tls_cert_file": "server.pem"}, map[string]string{"ORBITBEACON_TLS_KEY_FILE": "server.key"}, nil},
		{"file key and flag certificate", map[string]any{"tls_key_file": "server.key"}, nil, map[string]string{"tls_cert_file": "server.pem"}},
		{"environment certificate and flag key", nil, map[string]string{"ORBITBEACON_TLS_CERT_FILE": "server.pem"}, map[string]string{"tls_key_file": "server.key"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]any{"data_dir": t.TempDir()}
			for key, value := range tt.file {
				values[key] = value
			}
			got, err := config.LoadServer(config.LoadOptions{File: configFile(t, values), LookupEnv: lookup(tt.env), Flags: tt.flags})
			if err != nil {
				t.Fatalf("complete resolved TLS pair rejected: %v", err)
			}
			if got.TLSCertFile != "server.pem" || got.TLSKeyFile != "server.key" {
				t.Errorf("resolved TLS pair = %q, %q", got.TLSCertFile, got.TLSKeyFile)
			}
		})
	}
	for _, key := range []string{"tls_cert_file", "tls_key_file"} {
		if _, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(nil), Flags: map[string]string{key: "unpaired.pem"}}); err == nil {
			t.Errorf("accepted incomplete resolved TLS pair with only %s", key)
		}
	}
}

func TestAgentInstallPolicyRejectsNonRootServerURL(t *testing.T) {
	base := t.TempDir()
	for _, serverURL := range []string{
		"https://beacon.example.test/prefix", "https://beacon.example.test/prefix/",
		"https://beacon.example.test//", "https://beacon.example.test/%2F",
		"https://beacon.example.test/?query=1", "https://beacon.example.test/?",
		"https://user@beacon.example.test/",
	} {
		t.Run(serverURL, func(t *testing.T) {
			policy := config.InstallPolicy{ServerURL: serverURL, StateDir: filepath.Join(base, "state"), RuntimeDir: filepath.Join(base, "run"), TrustKeyFile: filepath.Join(base, "trust.pub"), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
			if _, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy); err == nil {
				t.Fatalf("agent accepted non-root or unsafe server_url %q", serverURL)
			}
		})
	}
}

func TestAgentStateDirectoryHasOneAbsoluteLocationAcrossWorkingDirectories(t *testing.T) {
	base := t.TempDir()
	absolute := filepath.Join(base, "fixed-state")
	for _, directory := range []string{"shell-start", "service-start"} {
		t.Run(directory, func(t *testing.T) {
			cwd := filepath.Join(base, directory)
			if err := os.Mkdir(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			for _, stateDir := range []string{"state", "./state", "../state", absolute} {
				policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: stateDir, RuntimeDir: filepath.Join(base, "runtime"), TrustKeyFile: filepath.Join(base, "trust.pub"), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
				resolved, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy)
				if stateDir != absolute {
					if err == nil {
						t.Fatalf("working-directory-dependent state path accepted: %q", stateDir)
					}
					continue
				}
				if err != nil || resolved.StateDir != absolute {
					t.Fatalf("absolute state location changed with cwd: %q err=%v", resolved.StateDir, err)
				}
			}
			if _, err := os.Stat(filepath.Join(cwd, "state")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("policy loading created a working-directory identity location: %v", err)
			}
		})
	}
}
