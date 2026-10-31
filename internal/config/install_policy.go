package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxInstallPolicySize = 64 << 10

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
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var policy *InstallPolicy
	if err := decoder.Decode(&policy); err != nil {
		return InstallPolicy{}, fmt.Errorf("解码安装策略失败: %w", err)
	}
	if policy == nil {
		return InstallPolicy{}, errors.New("安装策略必须是一个 JSON 对象")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return InstallPolicy{}, errors.New("安装策略只能包含一个 JSON 对象")
		}
		return InstallPolicy{}, fmt.Errorf("解码安装策略失败: %w", err)
	}
	if limited.N == 0 {
		return InstallPolicy{}, errors.New("安装策略超过 64 KiB 大小限制")
	}
	return *policy, nil
}
