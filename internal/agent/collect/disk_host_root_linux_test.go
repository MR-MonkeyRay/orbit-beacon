//go:build linux

package collect

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestDiskCollectionResolvesPathsUnderHostRoot(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		hostRoot   bool
		wantLabel  string
		wantPath   func(string) string
	}{
		{
			name:       "native path unchanged",
			configured: []string{"/var/lib/orbit"},
			wantLabel:  "/var/lib/orbit",
			wantPath:   func(string) string { return "/var/lib/orbit" },
		},
		{
			name:      "default root uses host mount",
			hostRoot:  true,
			wantLabel: "/",
			wantPath:  func(root string) string { return root },
		},
		{
			name:       "configured path uses host mount",
			configured: []string{"/var/lib/orbit"},
			hostRoot:   true,
			wantLabel:  "/var/lib/orbit",
			wantPath:   func(root string) string { return filepath.Join(root, "var/lib/orbit") },
		},
		{
			name:       "parent components stay under host mount",
			configured: []string{"/../../var/lib/orbit"},
			hostRoot:   true,
			wantLabel:  "/../../var/lib/orbit",
			wantPath:   func(root string) string { return filepath.Join(root, "var/lib/orbit") },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := ""
			if test.hostRoot {
				root = t.TempDir()
			}
			t.Setenv("HOST_ROOT", root)
			collector, err := New(Options{Config: v1.RuntimeSettings{
				ReportIntervalSeconds: 10,
				DiskIntervalSeconds:   60,
				DiskPaths:             test.configured,
			}})
			if err != nil {
				t.Fatal(err)
			}
			var readPath string
			collector.diskUsage = func(_ context.Context, path string) (*disk.UsageStat, error) {
				readPath = path
				return &disk.UsageStat{Total: 100, Used: 25}, nil
			}
			metrics := v1.Metrics{MetricStatus: make(map[string]string), Sources: make(map[string]v1.SourceMetadata)}
			collector.collectDisks(context.Background(), &metrics)

			if readPath != test.wantPath(root) {
				t.Fatalf("disk usage path=%q want %q", readPath, test.wantPath(root))
			}
			if len(metrics.Disks) != 1 || metrics.Disks[0].Path != test.wantLabel {
				t.Fatalf("reported disks=%+v want path %q", metrics.Disks, test.wantLabel)
			}
			if metrics.MetricStatus["disk:"+test.wantLabel] != "ok" {
				t.Fatalf("disk status=%q", metrics.MetricStatus["disk:"+test.wantLabel])
			}
		})
	}
}
