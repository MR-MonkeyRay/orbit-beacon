package buildinfo

import "runtime"

// These variables are replaced by release builds with -ldflags -X. Development
// binaries retain explicit non-empty values so diagnostics stay machine-readable.
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
