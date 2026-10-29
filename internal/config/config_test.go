package config_test

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
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

func TestServerTrustedProxiesPrecedenceAndParsing(t *testing.T) {
	file := configFile(t, map[string]any{"trusted_proxies": []string{"192.0.2.10", "2001:db8::/48"}})
	for _, tt := range []struct {
		name  string
		file  string
		env   map[string]string
		flags map[string]string
		want  []netip.Prefix
	}{
		{name: "default is empty", want: []netip.Prefix{}},
		{name: "JSON array", file: file, want: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32"), netip.MustParsePrefix("2001:db8::/48")}},
		{name: "environment comma list overrides JSON", file: file, env: map[string]string{"ORBITBEACON_TRUSTED_PROXIES": "10.0.0.0/8, 2001:db8:1::1"}, want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:1::1/128")}},
		{name: "flag overrides environment", file: file, env: map[string]string{"ORBITBEACON_TRUSTED_PROXIES": "10.0.0.0/8"}, flags: map[string]string{"trusted_proxies": "172.16.4.9,fd00::/8"}, want: []netip.Prefix{netip.MustParsePrefix("172.16.4.9/32"), netip.MustParsePrefix("fd00::/8")}},
		{name: "mapped IPv4 is normalized", env: map[string]string{"ORBITBEACON_TRUSTED_PROXIES": "::ffff:192.0.2.10,::ffff:198.51.100.0/120"}, want: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32"), netip.MustParsePrefix("198.51.100.0/24")}},
		{name: "empty environment clears JSON", file: file, env: map[string]string{"ORBITBEACON_TRUSTED_PROXIES": ""}, want: []netip.Prefix{}},
		{name: "empty flag clears environment", env: map[string]string{"ORBITBEACON_TRUSTED_PROXIES": "10.0.0.0/8"}, flags: map[string]string{"trusted_proxies": ""}, want: []netip.Prefix{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.LoadServer(config.LoadOptions{File: tt.file, LookupEnv: lookup(tt.env), Flags: tt.flags})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.TrustedProxies) != len(tt.want) {
				t.Fatalf("trusted proxies = %v, want %v", got.TrustedProxies, tt.want)
			}
			for i := range tt.want {
				if got.TrustedProxies[i] != tt.want[i] {
					t.Errorf("trusted proxy %d = %v, want %v", i, got.TrustedProxies[i], tt.want[i])
				}
			}
		})
	}
}

func TestServerTrustedProxiesRejectInvalidValues(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
		text  string
	}{
		{name: "IPv4 default route", value: []string{"0.0.0.0/0"}, text: "0.0.0.0/0"},
		{name: "IPv6 default route", value: []string{"::/0"}, text: "::/0"},
		{name: "malformed address", value: []string{"192.0.2.999"}, text: "192.0.2.999"},
		{name: "malformed prefix", value: []string{"10.0.0.0/33"}, text: "10.0.0.0/33"},
		{name: "scoped address", value: []string{"fe80::1%eth0"}, text: "fe80::1%eth0"},
		{name: "mapped IPv4 prefix covers non-IPv4 addresses", value: []string{"::ffff:0:0/96"}, text: "::ffff:0:0/96"},
		{name: "empty list entry", value: []string{""}, text: ",10.0.0.1"},
		{name: "JSON string", value: "10.0.0.1", text: "10.0.0.1"},
		{name: "JSON null", value: nil, text: "10.0.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := config.LoadServer(config.LoadOptions{File: configFile(t, map[string]any{"trusted_proxies": tt.value}), LookupEnv: lookup(nil)}); err == nil {
				t.Fatal("accepted invalid trusted_proxies JSON value")
			}
			if tt.name == "JSON string" || tt.name == "JSON null" {
				return
			}
			for _, source := range []string{"environment", "flag"} {
				opts := config.LoadOptions{LookupEnv: lookup(nil)}
				if source == "environment" {
					opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_TRUSTED_PROXIES": tt.text})
				} else {
					opts.Flags = map[string]string{"trusted_proxies": tt.text}
				}
				if _, err := config.LoadServer(opts); err == nil {
					t.Fatalf("accepted invalid trusted_proxies from %s", source)
				}
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
		{"unsupported public URL scheme", "public_url", "ORBITBEACON_PUBLIC_URL", "ftp://example.test"},
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
func TestRemovedConfigurationKeysAreRejected(t *testing.T) {
	if _, err := config.LoadServer(config.LoadOptions{
		File:      configFile(t, map[string]any{"trusted_proxy_cidrs": []string{"127.0.0.0/8"}}),
		LookupEnv: lookup(nil),
	}); err == nil {
		t.Fatal("accepted removed trusted_proxy_cidrs configuration")
	}
	policy := config.InstallPolicy{
		ServerURL:   "https://beacon.example.test",
		StateDir:    t.TempDir(),
		ExpectedUID: 1000,
		ExpectedGID: 1000,
		InstallMode: "native",
	}
	for _, key := range []string{"runtime_dir", "trust_key_file"} {
		t.Run(key, func(t *testing.T) {
			if _, err := config.LoadAgent(config.LoadOptions{
				File:      configFile(t, map[string]any{key: "/tmp/removed"}),
				LookupEnv: lookup(nil),
			}, &policy); err == nil {
				t.Fatalf("accepted removed %s configuration", key)
			}
		})
	}
}

func TestAgentInstallPolicyCannotBeOverridden(t *testing.T) {
	base := t.TempDir()
	policy := config.InstallPolicy{
		ServerURL:       "https://beacon.example.test",
		CAFile:          filepath.Join(base, "ca.pem"),
		StateDir:        filepath.Join(base, "state"),
		ExpectedUID:     1000,
		ExpectedGID:     1000,
		ExpectedAccount: "orbitbeacon",
		InstallMode:     "native",
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
		{"expected_uid", "ORBITBEACON_EXPECTED_UID", 1001, "1001"},
		{"expected_gid", "ORBITBEACON_EXPECTED_GID", 1001, "1001"},
		{"expected_account", "ORBITBEACON_EXPECTED_ACCOUNT", "Administrator", "Administrator"},
		{"install_mode", "ORBITBEACON_INSTALL_MODE", "docker", "docker"},
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
	if got.ServerURL != policy.ServerURL || got.ExpectedUID != 1000 {
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
		{"public URL", "public_url", "ORBITBEACON_PUBLIC_URL", "ftp://invalid.test", "ftp://invalid.test", "https://beacon.example.test", "https://beacon.example.test"},
		{"data directory", "data_dir", "ORBITBEACON_DATA_DIR", "file:unsafe.db", "file:unsafe.db", "/var/lib/orbitbeacon", "/var/lib/orbitbeacon"},
		{"listen address", "listen_addr", "ORBITBEACON_LISTEN_ADDR", "not-an-address", "not-an-address", "127.0.0.1:8181", "127.0.0.1:8181"},
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

func TestServerRejectsRemovedTLSConfiguration(t *testing.T) {
	for _, key := range []string{"tls_cert_file", "tls_key_file"} {
		t.Run(key, func(t *testing.T) {
			if _, err := config.LoadServer(config.LoadOptions{
				File: configFile(t, map[string]any{key: "server.pem"}), LookupEnv: lookup(nil),
			}); err == nil {
				t.Fatal("accepted removed TLS JSON key")
			}
			if _, err := config.LoadServer(config.LoadOptions{
				LookupEnv: lookup(nil), Flags: map[string]string{key: "server.pem"},
			}); err == nil {
				t.Fatal("accepted removed TLS flag key")
			}
		})
	}
	var lookedUpTLS bool
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(key string) (string, bool) {
		if key == "ORBITBEACON_TLS_CERT_FILE" || key == "ORBITBEACON_TLS_KEY_FILE" {
			lookedUpTLS = true
			return "server.pem", true
		}
		return "", false
	}})
	if err != nil || lookedUpTLS {
		t.Fatalf("removed TLS environment consulted: %v, err=%v", lookedUpTLS, err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "tls_cert_file") || strings.Contains(string(encoded), "tls_key_file") {
		t.Fatalf("server configuration still exports removed TLS settings: %s", encoded)
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
			policy := config.InstallPolicy{ServerURL: serverURL, StateDir: filepath.Join(base, "state"), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
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
				policy := config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: stateDir, ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"}
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

func TestHTTPTransportConfiguration(t *testing.T) {
	t.Run("default public origin", func(t *testing.T) {
		cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(nil)})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PublicURL != "http://localhost:8080" {
			t.Fatalf("default public URL = %q, want http://localhost:8080", cfg.PublicURL)
		}
	})
	t.Run("HTTP public URL from every source", func(t *testing.T) {
		const publicURL = "http://192.0.2.10:8080"
		for _, source := range []string{"JSON", "environment", "flag"} {
			t.Run(source, func(t *testing.T) {
				opts := config.LoadOptions{LookupEnv: lookup(nil)}
				switch source {
				case "JSON":
					opts.File = configFile(t, map[string]any{"public_url": publicURL})
				case "environment":
					opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_PUBLIC_URL": publicURL})
				case "flag":
					opts.Flags = map[string]string{"public_url": publicURL}
				}
				cfg, err := config.LoadServer(opts)
				if err != nil || cfg.PublicURL != publicURL {
					t.Fatalf("HTTP public URL rejected or changed: %q, err=%v", cfg.PublicURL, err)
				}
			})
		}
	})
	t.Run("canonical origins", func(t *testing.T) {
		for raw, want := range map[string]string{
			"http://EXAMPLE.test:80/":             "http://example.test",
			"HTTP://EXAMPLE.test:80/":             "http://example.test",
			"http://example.test:00080/dashboard": "http://example.test",
			"http://example.test:443":             "http://example.test:443",
			"http://example.test:8080":            "http://example.test:8080",
			"http://[2001:0DB8::1]:80/":           "http://[2001:db8::1]",
			"http://[2001:db8::1]:8080":           "http://[2001:db8::1]:8080",
			"https://EXAMPLE.test:443/":           "https://example.test",
			"https://example.test:80":             "https://example.test:80",
			"https://[2001:db8::1]:443":           "https://[2001:db8::1]",
		} {
			t.Run(raw, func(t *testing.T) {
				got, err := config.CanonicalOrigin(raw)
				if err != nil || got != want {
					t.Fatalf("CanonicalOrigin(%q) = %q, %v; want %q", raw, got, err, want)
				}
			})
		}
	})
	t.Run("agent transport and CA", func(t *testing.T) {
		for _, tt := range []struct {
			serverURL string
			caFile    string
			wantError bool
		}{
			{"http://beacon.example.test", "", false},
			{"http://192.0.2.10:8080/", "", false},
			{"http://[2001:db8::1]:8080/", "", false},
			{"https://beacon.example.test/", "", false},
			{"https://beacon.example.test/", "ca.pem", false},
			{"http://beacon.example.test/", "ca.pem", true},
			{"HTTP://beacon.example.test/", "ca.pem", true},
		} {
			t.Run(tt.serverURL+"/"+tt.caFile, func(t *testing.T) {
				policy := config.InstallPolicy{ServerURL: tt.serverURL, CAFile: tt.caFile, StateDir: t.TempDir(), InstallMode: "foreground"}
				got, err := config.LoadAgent(config.LoadOptions{LookupEnv: lookup(nil)}, &policy)
				if tt.wantError {
					if err == nil || !strings.Contains(err.Error(), "ca_file") || !strings.Contains(err.Error(), "HTTPS") {
						t.Fatalf("HTTP with explicit CA must explain that ca_file requires HTTPS, got %v", err)
					}
					return
				}
				if err != nil || got.ServerURL != tt.serverURL || got.CAFile != tt.caFile {
					t.Fatalf("agent transport config changed: %+v, %v", got, err)
				}
			})
		}
	})
}

func TestServerURLValidation(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			for _, authority := range []string{"example.test", "EXAMPLE.test:8080", "192.0.2.10:80", "[2001:db8::1]:443"} {
				for _, path := range []string{"", "/"} {
					raw := scheme + "://" + authority + path
					if err := config.ValidateServerURL(raw); err != nil {
						t.Errorf("valid server URL %q rejected: %v", raw, err)
					}
				}
			}
			for _, suffix := range []string{"/prefix", "/prefix/", "//", "/%2F", "/%2f", "/%41", "/?query=1", "/?", "/#fragment", "/#"} {
				raw := scheme + "://example.test" + suffix
				if err := config.ValidateServerURL(raw); err == nil {
					t.Errorf("unsafe or non-root server URL %q accepted", raw)
				}
			}
			for _, authority := range []string{"", "user@example.test", "user:password@example.test", "bad_host.test", "127.1", "0x7f000001", "example.test:", "example.test:65536", "[fe80::1%25eth0]", "[::ffff:192.0.2.10]"} {
				raw := scheme + "://" + authority
				if _, err := config.CanonicalOrigin(raw); err == nil {
					t.Errorf("unsafe origin %q accepted", raw)
				}
				if err := config.ValidateServerURL(raw); err == nil {
					t.Errorf("unsafe server URL %q accepted", raw)
				}
			}
		})
	}
	for _, raw := range []string{"", "/", "example.test", "//example.test", "ftp://example.test", "http:example.test", "https:example.test"} {
		if _, err := config.CanonicalOrigin(raw); err == nil {
			t.Errorf("non-HTTP(S) absolute origin %q accepted", raw)
		}
		if err := config.ValidateServerURL(raw); err == nil {
			t.Errorf("non-HTTP(S) absolute server URL %q accepted", raw)
		}
	}
}
