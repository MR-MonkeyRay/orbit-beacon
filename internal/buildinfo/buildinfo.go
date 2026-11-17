// Package buildinfo 暴露编译期注入的二进制版本与运行平台信息。
package buildinfo

import "runtime"

// 这些变量在发布构建时通过 -ldflags -X 替换。开发二进制保留显式的非空值，
// 保证诊断信息机器可读。
var (
	Version   = "development"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Info 描述当前二进制的构建与运行环境元数据。
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Current 返回当前进程编译期版本与运行时 OS/Arch 信息。
func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildDate: BuildDate, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
}
