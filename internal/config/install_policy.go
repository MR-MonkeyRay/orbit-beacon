package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxInstallPolicySize = 64 << 10

// ReadInstallPolicy decodes only the securely opened, administrator-owned file.
// expectedOwner is the trusted administrator's UID, not the runtime agent UID.
func ReadInstallPolicy(path string, expectedOwner int) (InstallPolicy, error) {
	if path == "" || expectedOwner < 0 {
		return InstallPolicy{}, errors.New("install policy requires a path and a nonnegative owner UID")
	}
	file, err := openInstallPolicy(path, expectedOwner)
	if err != nil {
		return InstallPolicy{}, fmt.Errorf("open install policy: %w", err)
	}
	defer file.Close()

	limited := &io.LimitedReader{R: file, N: maxInstallPolicySize + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var policy *InstallPolicy
	if err := decoder.Decode(&policy); err != nil {
		return InstallPolicy{}, fmt.Errorf("decode install policy: %w", err)
	}
	if policy == nil {
		return InstallPolicy{}, errors.New("install policy must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return InstallPolicy{}, errors.New("install policy must contain only one JSON object")
		}
		return InstallPolicy{}, fmt.Errorf("decode install policy: %w", err)
	}
	if limited.N == 0 {
		return InstallPolicy{}, errors.New("install policy exceeds the 64 KiB size limit")
	}
	return *policy, nil
}
