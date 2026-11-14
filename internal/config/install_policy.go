package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxInstallPolicySize = 64 << 10

const defaultGitHubUpdateBaseURL = "https://github.com/mr-monkeyray/orbit-beacon/releases/download"

// ReadInstallPolicy 只解码已安全打开、归管理员所有的文件。
// expectedOwner 是受信任管理员的 UID，不是 Agent 运行账户的 UID。
func ReadInstallPolicy(path string, expectedOwner int) (InstallPolicy, error) {
	if path == "" || expectedOwner < 0 {
		return InstallPolicy{}, errors.New("安装策略需要路径和非负属主 UID")
	}
	file, err := openInstallPolicy(path, expectedOwner)
	if err != nil {
		return InstallPolicy{}, fmt.Errorf("打开安装策略失败: %w", err)
	}
	defer file.Close()

	limited := &io.LimitedReader{R: file, N: maxInstallPolicySize + 1}
	data, err := decodeYAML(limited)
	if limited.N == 0 {
		return InstallPolicy{}, errors.New("安装策略超过 64 KiB 大小限制")
	}
	if err != nil {
		return InstallPolicy{}, fmt.Errorf("解码安装策略失败: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded struct {
		InstallPolicy
		UpdateBaseURL json.RawMessage `json:"update_base_url"`
	}
	if err := decoder.Decode(&decoded); err != nil {
		return InstallPolicy{}, errors.New("安装策略字段或类型无效")
	}
	policy := decoded.InstallPolicy
	policy.UpdateBaseURL = defaultGitHubUpdateBaseURL
	if len(decoded.UpdateBaseURL) != 0 {
		var base string
		if err := json.Unmarshal(decoded.UpdateBaseURL, &base); err != nil || base == "" {
			return InstallPolicy{}, errors.New("update_base_url 必须是非空 HTTPS 发布根地址")
		}
		policy.UpdateBaseURL = base
	}
	return NormalizeUpdatePolicy(policy)
}

// NormalizeUpdatePolicy 校验并规范化本地可信发布源，不修改原策略。
func NormalizeUpdatePolicy(policy InstallPolicy) (InstallPolicy, error) {
	if policy.UpdateBaseURL == "" {
		policy.UpdateBaseURL = defaultGitHubUpdateBaseURL
	}
	normalized, err := NormalizeUpdateBaseURL(policy.UpdateBaseURL)
	if err != nil {
		return InstallPolicy{}, fmt.Errorf("update_base_url: %w", err)
	}
	policy.UpdateBaseURL = normalized
	return policy, nil
}
