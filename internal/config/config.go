package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	v1 "orbitbeacon/internal/protocol/v1"
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
	MaxNodes                         int            `json:"max_nodes"`
	WebUpdateIntervalSeconds         int            `json:"web_update_interval_seconds"`
	TrustedProxies                   []netip.Prefix `json:"-"`
}

// InstallPolicy 是归管理员所有的配置。每个来源都独立校验，
// 防止后续覆盖掩盖策略冲突。
type InstallPolicy struct {
	ServerURL       string `json:"server_url"`
	CAFile          string `json:"ca_file"`
	StateDir        string `json:"state_dir"`
	ExpectedUID     int    `json:"expected_uid"`
	ExpectedGID     int    `json:"expected_gid"`
	ExpectedAccount string `json:"expected_account"`
	InstallMode     string `json:"install_mode"`
}

type Agent struct {
	InstallPolicy
	v1.RuntimeSettings
}

func sources(opts LoadOptions, keys []string) ([]map[string]json.RawMessage, error) {
	file := make(map[string]json.RawMessage)
	if opts.File != "" {
		data, err := os.ReadFile(opts.File)
		if err != nil {
			return nil, fmt.Errorf("读取配置失败: %w", err)
		}
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("解码配置失败: %w", err)
		}
		if file == nil {
			return nil, errors.New("配置必须是对象")
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
			return nil, fmt.Errorf("未知配置键 %q", key)
		}
	}
	for key := range opts.Flags {
		if !known[key] {
			return nil, fmt.Errorf("未知 flag 键 %q", key)
		}
	}
	return []map[string]json.RawMessage{file, environment, flags}, nil
}

func encodeValue(key, value string) (json.RawMessage, error) {
	switch key {
	case "max_nodes", "expected_uid", "expected_gid", "report_interval_seconds", "disk_interval_seconds", "web_update_interval_seconds":
		n, err := strconv.Atoi(value)
		if err != nil {
			// 格式错误的值先按字符串保留，待来源优先级确定后，
			// 解码到整型字段时再拒绝。
			return json.Marshal(value)
		}
		return json.Marshal(n)
	case "access_log", "allow_insecure_webhooks", "notification_allow_private_networks":
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			// 保留格式错误的值，待来源优先级确定后，
			// 解码时再报告配置类型错误。
			return json.Marshal(value)
		}
		return json.Marshal(enabled)
	case "interfaces", "disk_paths", "probe_targets":
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("%s 必须是 JSON 字符串数组", key)
		}
		return json.RawMessage(value), nil
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
	cfg := Server{ListenAddr: "127.0.0.1:8080", DataDir: "./data", PublicURL: "http://localhost:8080", MaxNodes: 500, WebUpdateIntervalSeconds: 10}
	layers, err := sources(opts, []string{"allow_insecure_webhooks", "notification_allow_private_networks", "listen_addr", "metrics_listen", "access_log", "data_dir", "public_url", "max_nodes", "web_update_interval_seconds", "trusted_proxies"})
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
			return Server{}, fmt.Errorf("%s 不能为 null", key)
		}
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		return Server{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Server{}, fmt.Errorf("配置无效: %w", err)
	}
	if value, ok := resolved["trusted_proxies"]; ok {
		var entries []string
		if err := json.Unmarshal(value, &entries); err != nil {
			return Server{}, fmt.Errorf("配置无效: trusted_proxies 必须是字符串数组: %w", err)
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
			return nil, fmt.Errorf("trusted_proxies: 地址或前缀无效 %q", entry)
		}
		if address, err := netip.ParseAddr(entry); err == nil {
			if address.Zone() != "" {
				return nil, fmt.Errorf("trusted_proxies: 不支持带作用域的地址 %q", entry)
			}
			address = address.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil || prefix.Addr().Zone() != "" {
			return nil, fmt.Errorf("trusted_proxies: 地址或前缀无效 %q", entry)
		}
		if prefix.Bits() == 0 {
			return nil, fmt.Errorf("trusted_proxies: 前缀 %q 过宽", entry)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() <= 96 {
				return nil, fmt.Errorf("trusted_proxies: 映射的 IPv4 前缀 %q 过宽", entry)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// CanonicalOrigin 校验绝对 HTTP 或 HTTPS URL，并返回其浏览器 Origin。
// 允许 URL 路径，但不允许凭据、查询和片段。
// DNS 名仅限 ASCII；不支持的主机形式直接拒绝，避免产生与浏览器
// 序列化结果不一致的 Origin。
func CanonicalOrigin(raw string) (string, error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Opaque != "" {
		return nil, errors.New("URL 必须是绝对 HTTP 或 HTTPS，且不含凭据、查询或片段")
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasPrefix(u.Host, "[") {
		address, err := netip.ParseAddr(host)
		if err != nil || !address.Is6() || address.Is4In6() || address.Zone() != "" {
			return nil, errors.New("URL 必须包含受支持的 IPv6 地址")
		}
		host = "[" + address.String() + "]"
	} else if address, err := netip.ParseAddr(host); err == nil {
		if !address.Is4() {
			return nil, errors.New("URL 中的 IPv6 地址必须加方括号")
		}
		host = address.String()
	} else if !validHostname(host) {
		return nil, errors.New("URL 必须包含有效的 ASCII 主机名或 IP 地址")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("URL 端口不能为空")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return nil, errors.New("URL 端口必须在 0 到 65535 之间")
		}
		defaultPort := (u.Scheme == "http" && number == 80) || (u.Scheme == "https" && number == 443)
		if !defaultPort {
			host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.FormatUint(number, 10))
		}
	}
	u.Host = host
	return u, nil
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
	// 浏览器把以数字结尾的名字当作 IPv4，包括十六进制和缩写形式。
	// 上面只接受规范的 IPv4 形式。
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "" {
		return false
	}
	return true
}

// ValidateServerURL 只接受 HTTP 或 HTTPS Origin，可选末尾的根路径斜杠 /。
// Agent 端点使用绝对路径，不支持 URL 路径前缀。
func ValidateServerURL(raw string) error {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return err
	}
	if (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return errors.New("URL 必须位于根路径，且不含编码路径或前缀")
	}
	return nil
}

// ValidatePath 拒绝 SQLite DSN 和网络 URI；存储一律使用本地文件。
func ValidatePath(path string) error {
	if strings.TrimSpace(path) == "" || strings.Contains(path, ":") || strings.ContainsAny(path, "?\x00") {
		return errors.New("路径必须是本地文件系统路径，不能是 URI 或 SQLite DSN")
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
		// 指标绝不能与公开监听共用地址。
		if cfg.MetricsListen == cfg.ListenAddr && port != "0" {
			return errors.New("metrics_listen: 必须与 listen_addr 不同")
		}
	}
	if err := ValidatePath(cfg.DataDir); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	if _, err := CanonicalOrigin(cfg.PublicURL); err != nil {
		return fmt.Errorf("public_url: %w", err)
	}
	if cfg.MaxNodes < 1 {
		return errors.New("max_nodes 必须为正数")
	}
	if cfg.WebUpdateIntervalSeconds < 2 || cfg.WebUpdateIntervalSeconds > 60 {
		return errors.New("web_update_interval_seconds 必须在 2 到 60 之间")
	}
	return nil
}

func LoadAgent(opts LoadOptions, policy *InstallPolicy) (Agent, error) {
	if policy == nil {
		return Agent{}, errors.New("必须提供归管理员所有的安装策略")
	}
	keys := []string{"server_url", "ca_file", "state_dir", "expected_uid", "expected_gid", "expected_account", "install_mode", "report_interval_seconds", "disk_interval_seconds", "interfaces", "disk_paths", "probe_targets"}
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
	settings := v1.DefaultRuntimeSettings()
	resolved := make(map[string]json.RawMessage)
	for _, layer := range layers {
		for key, value := range layer {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return Agent{}, fmt.Errorf("%s 不能为 null", key)
			}
			// 即使更高优先级的来源为同一运行时键提供了其他值，
			// 每个本地来源自身也必须类型正确且有效。
			if _, ok := protected[key]; !ok {
				candidate := v1.DefaultRuntimeSettings()
				encoded, err := json.Marshal(map[string]json.RawMessage{key: value})
				if err != nil {
					return Agent{}, err
				}
				if err := json.Unmarshal(encoded, &candidate); err != nil {
					return Agent{}, fmt.Errorf("%s 的运行时配置类型无效", key)
				}
				if err := candidate.Validate(); err != nil {
					return Agent{}, err
				}
			}
			if _, ok := protected[key]; !ok {
				resolved[key] = value
				continue
			}
			var actual, expected any
			if err := json.Unmarshal(value, &actual); err != nil {
				return Agent{}, err
			}
			if err := json.Unmarshal(protected[key], &expected); err != nil {
				return Agent{}, err
			}
			if !reflect.DeepEqual(actual, expected) {
				return Agent{}, fmt.Errorf("install_policy_conflict: %s", key)
			}
		}
	}
	data, err = json.Marshal(resolved)
	if err != nil {
		return Agent{}, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return Agent{}, errors.New("运行时配置类型无效")
	}
	if err := settings.Validate(); err != nil {
		return Agent{}, err
	}
	if err := ValidateServerURL(policy.ServerURL); err != nil {
		return Agent{}, fmt.Errorf("server_url: %w", err)
	}
	scheme, _, _ := strings.Cut(policy.ServerURL, ":")
	if strings.EqualFold(scheme, "http") && policy.CAFile != "" {
		return Agent{}, errors.New("ca_file 仅适用于 HTTPS 的 server_url；HTTP 不使用 TLS 证书")
	}
	if err := ValidatePath(policy.StateDir); err != nil {
		return Agent{}, err
	}
	if !filepath.IsAbs(policy.StateDir) {
		return Agent{}, errors.New("state_dir 必须是绝对文件系统路径")
	}
	if policy.ExpectedUID < 0 || policy.ExpectedGID < 0 {
		return Agent{}, errors.New("期望的 UID/GID 不能为负数")
	}
	if policy.InstallMode != "native" && policy.InstallMode != "docker" && policy.InstallMode != "foreground" {
		return Agent{}, errors.New("install_mode 必须是 native、docker 或 foreground")
	}
	return Agent{InstallPolicy: *policy, RuntimeSettings: settings}, nil
}
