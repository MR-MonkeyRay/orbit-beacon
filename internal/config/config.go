package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type LoadOptions struct {
	File      string
	LookupEnv func(string) (string, bool)
	Flags     map[string]string
}

type Server struct {
	AllowInsecureWebhooks            bool           `json:"allow_insecure_webhooks"`
	NotificationAllowPrivateNetworks bool           `json:"notification_allow_private_networks"`
	ListenAddr                       string         `json:"listen_addr"`
	MetricsListen                    string         `json:"metrics_listen"`
	AccessLog                        bool           `json:"access_log"`
	DataDir                          string         `json:"data_dir"`
	PublicURL                        string         `json:"public_url"`
	TLSCertFile                      string         `json:"tls_cert_file"`
	TLSKeyFile                       string         `json:"tls_key_file"`
	MaxNodes                         int            `json:"max_nodes"`
	TrustedProxies                   []netip.Prefix `json:"-"`
}

// InstallPolicy is administrator-owned configuration. Every source is checked
// independently, so a later matching override cannot hide a policy conflict.
type InstallPolicy struct {
	ServerURL       string `json:"server_url"`
	CAFile          string `json:"ca_file"`
	StateDir        string `json:"state_dir"`
	ExpectedUID     int    `json:"expected_uid"`
	ExpectedGID     int    `json:"expected_gid"`
	ExpectedAccount string `json:"expected_account"`
	InstallMode     string `json:"install_mode"`
}

type Agent struct{ InstallPolicy }

func sources(opts LoadOptions, keys []string) ([]map[string]json.RawMessage, error) {
	file := make(map[string]json.RawMessage)
	if opts.File != "" {
		data, err := os.ReadFile(opts.File)
		if err != nil {
			return nil, fmt.Errorf("read configuration: %w", err)
		}
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("decode configuration: %w", err)
		}
		if file == nil {
			return nil, errors.New("configuration must be an object")
		}
	}
	known := make(map[string]bool, len(keys))
	environment := make(map[string]json.RawMessage)
	flags := make(map[string]json.RawMessage)
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, key := range keys {
		known[key] = true
		if value, ok := lookup("ORBITBEACON_" + strings.ToUpper(key)); ok {
			encoded, err := encodeValue(key, value)
			if err != nil {
				return nil, err
			}
			environment[key] = encoded
		}
		if value, ok := opts.Flags[key]; ok {
			encoded, err := encodeValue(key, value)
			if err != nil {
				return nil, err
			}
			flags[key] = encoded
		}
	}
	for key := range file {
		if !known[key] {
			return nil, fmt.Errorf("unknown configuration key %q", key)
		}
	}
	for key := range opts.Flags {
		if !known[key] {
			return nil, fmt.Errorf("unknown flag key %q", key)
		}
	}
	return []map[string]json.RawMessage{file, environment, flags}, nil
}

func encodeValue(key, value string) (json.RawMessage, error) {
	switch key {
	case "max_nodes", "expected_uid", "expected_gid":
		n, err := strconv.Atoi(value)
		if err != nil {
			// Keep malformed values typed as strings until source precedence is
			// resolved. Decoding a selected value into its integer field rejects it.
			return json.Marshal(value)
		}
		return json.Marshal(n)
	case "access_log", "allow_insecure_webhooks", "notification_allow_private_networks":
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			// Preserve the malformed value so decoding reports a typed
			// configuration error after source precedence is resolved.
			return json.Marshal(value)
		}
		return json.Marshal(enabled)
	case "trusted_proxies":
		if value == "" {
			return json.Marshal([]string{})
		}
		parts := strings.Split(value, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return json.Marshal(parts)
	default:
		return json.Marshal(value)
	}
}

func LoadServer(opts LoadOptions) (Server, error) {
	cfg := Server{ListenAddr: "127.0.0.1:8080", DataDir: "./data", PublicURL: "https://localhost", MaxNodes: 500}
	layers, err := sources(opts, []string{"allow_insecure_webhooks", "notification_allow_private_networks", "listen_addr", "metrics_listen", "access_log", "data_dir", "public_url", "tls_cert_file", "tls_key_file", "max_nodes", "trusted_proxies"})
	if err != nil {
		return Server{}, err
	}
	resolved := make(map[string]json.RawMessage)
	for _, layer := range layers {
		for key, value := range layer {
			resolved[key] = value
		}
	}
	for key, value := range resolved {
		if string(value) == "null" {
			return Server{}, fmt.Errorf("%s must not be null", key)
		}
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		return Server{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Server{}, fmt.Errorf("invalid configuration: %w", err)
	}
	if value, ok := resolved["trusted_proxies"]; ok {
		var entries []string
		if err := json.Unmarshal(value, &entries); err != nil {
			return Server{}, fmt.Errorf("invalid configuration: trusted_proxies must be an array of strings: %w", err)
		}
		cfg.TrustedProxies, err = parseTrustedProxies(entries)
		if err != nil {
			return Server{}, err
		}
	}
	return cfg, cfg.Validate()
}

func parseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		if entry == "" || strings.TrimSpace(entry) != entry {
			return nil, fmt.Errorf("trusted_proxies: invalid address or prefix %q", entry)
		}
		if address, err := netip.ParseAddr(entry); err == nil {
			if address.Zone() != "" {
				return nil, fmt.Errorf("trusted_proxies: scoped address %q is not supported", entry)
			}
			address = address.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil || prefix.Addr().Zone() != "" {
			return nil, fmt.Errorf("trusted_proxies: invalid address or prefix %q", entry)
		}
		if prefix.Bits() == 0 {
			return nil, fmt.Errorf("trusted_proxies: prefix %q is too broad", entry)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() <= 96 {
				return nil, fmt.Errorf("trusted_proxies: mapped IPv4 prefix %q is too broad", entry)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// CanonicalOrigin validates an absolute HTTPS URL and returns its browser Origin.
// URL paths are allowed, but credentials, queries and fragments are not.
// DNS names are ASCII-only; unsupported host forms are rejected rather than
// producing an Origin that differs from the browser's serialization.
func CanonicalOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Opaque != "" {
		return "", errors.New("URL must be absolute HTTPS without credentials, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasPrefix(u.Host, "[") {
		address, err := netip.ParseAddr(host)
		if err != nil || !address.Is6() || address.Is4In6() || address.Zone() != "" {
			return "", errors.New("URL must have a supported IPv6 address")
		}
		host = "[" + address.String() + "]"
	} else if address, err := netip.ParseAddr(host); err == nil {
		if !address.Is4() {
			return "", errors.New("URL IPv6 addresses must be bracketed")
		}
		host = address.String()
	} else if !validHostname(host) {
		return "", errors.New("URL must have a valid ASCII hostname or IP address")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("URL port must not be empty")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", errors.New("URL port must be between 0 and 65535")
		}
		if number != 443 {
			host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.FormatUint(number, 10))
		}
	}
	return "https://" + host, nil
}

func validHostname(host string) bool {
	name := strings.TrimSuffix(host, ".")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	// Browsers treat names ending in a number as IPv4, including hexadecimal
	// and shortened forms. Only the canonical IPv4 form is accepted above.
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "" {
		return false
	}
	return true
}

// ValidateHTTPS requires an HTTPS origin with at most a literal root slash.
// Agent endpoints are absolute paths; accepting a prefix would silently discard it.
func ValidateHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return errors.New("URL must be absolute HTTPS at its root without credentials, query or fragment")
	}
	return nil
}

// ValidatePath rejects SQLite DSNs and network URIs; storage always uses a local file.
func ValidatePath(path string) error {
	if strings.TrimSpace(path) == "" || strings.Contains(path, ":") || strings.ContainsAny(path, "?\x00") {
		return errors.New("path must be a local filesystem path, not a URI or SQLite DSN")
	}
	return nil
}

func (cfg Server) Validate() error {
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return fmt.Errorf("listen_addr: %w", err)
	}
	if cfg.MetricsListen != "" {
		_, port, err := net.SplitHostPort(cfg.MetricsListen)
		if err != nil {
			return fmt.Errorf("metrics_listen: %w", err)
		}
		// Metrics must never share the public listener.
		if cfg.MetricsListen == cfg.ListenAddr && port != "0" {
			return errors.New("metrics_listen: must differ from listen_addr")
		}
	}
	if err := ValidatePath(cfg.DataDir); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	if _, err := CanonicalOrigin(cfg.PublicURL); err != nil {
		return fmt.Errorf("public_url: %w", err)
	}
	if cfg.MaxNodes < 1 {
		return errors.New("max_nodes must be positive")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return errors.New("tls_cert_file and tls_key_file must be supplied together")
	}
	return nil
}

func LoadAgent(opts LoadOptions, policy *InstallPolicy) (Agent, error) {
	if policy == nil {
		return Agent{}, errors.New("administrator-owned install policy is required")
	}
	keys := []string{"server_url", "ca_file", "state_dir", "expected_uid", "expected_gid", "expected_account", "install_mode"}
	layers, err := sources(opts, keys)
	if err != nil {
		return Agent{}, err
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return Agent{}, err
	}
	var protected map[string]json.RawMessage
	if err := json.Unmarshal(data, &protected); err != nil {
		return Agent{}, err
	}
	for _, layer := range layers {
		for key, value := range layer {
			var actual, expected any
			if err := json.Unmarshal(value, &actual); err != nil {
				return Agent{}, err
			}
			if err := json.Unmarshal(protected[key], &expected); err != nil {
				return Agent{}, err
			}
			if actual != expected {
				return Agent{}, fmt.Errorf("install_policy_conflict: %s", key)
			}
		}
	}
	if err := ValidateHTTPS(policy.ServerURL); err != nil {
		return Agent{}, fmt.Errorf("server_url: %w", err)
	}
	if err := ValidatePath(policy.StateDir); err != nil {
		return Agent{}, err
	}
	if !filepath.IsAbs(policy.StateDir) {
		return Agent{}, errors.New("state_dir must be an absolute filesystem path")
	}
	if policy.ExpectedUID < 0 || policy.ExpectedGID < 0 {
		return Agent{}, errors.New("expected uid/gid must not be negative")
	}
	if policy.InstallMode != "native" && policy.InstallMode != "docker" && policy.InstallMode != "foreground" {
		return Agent{}, errors.New("install_mode must be native, docker or foreground")
	}
	return Agent{InstallPolicy: *policy}, nil
}
