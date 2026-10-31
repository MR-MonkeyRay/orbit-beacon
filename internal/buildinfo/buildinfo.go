package buildinfo

import "runtime"

// 这些变量在发布构建时通过 -ldflags -X 替换。开发二进制保留显式的非空值，
// 保证诊断信息机器可读。
var (
	Version   = "development"
	Commit    = "unknown"
	BuildDate = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildDate: BuildDate, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
}
