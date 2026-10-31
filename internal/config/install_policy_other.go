//go:build !unix

package config

import (
	"errors"
	"os"
)

func openInstallPolicy(path string, expectedOwner int) (*os.File, error) {
	return nil, errors.New("本平台不支持安全加载归管理员所有的安装策略")
}
