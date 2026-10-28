package service_test

import (
	"strings"
	"testing"

	"orbitbeacon/internal/agent/collect"
	v1 "orbitbeacon/internal/protocol/v1"
)

func runtimeSettings(interval int) v1.RuntimeSettings {
	settings := v1.DefaultRuntimeSettings()
	settings.ReportIntervalSeconds = interval
	settings.Interfaces = []string{"eth0"}
	settings.DiskPaths = []string{"/"}
	return settings
}

func TestRuntimeSettingsValidationMatchesCollectorApplication(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*v1.RuntimeSettings)
		valid  bool
	}{
		{"ordinary", func(*v1.RuntimeSettings) {}, true},
		{"automatic selection", func(c *v1.RuntimeSettings) { c.DiskPaths = []string{}; c.Interfaces = []string{} }, true},
		{"bounded interface", func(c *v1.RuntimeSettings) { c.Interfaces = []string{strings.Repeat("a", 255)} }, true},
		{"too frequent", func(c *v1.RuntimeSettings) { c.ReportIntervalSeconds = 4 }, false},
		{"too slow", func(c *v1.RuntimeSettings) { c.ReportIntervalSeconds = 61 }, false},
		{"disk frequency", func(c *v1.RuntimeSettings) { c.DiskIntervalSeconds = 59 }, false},
		{"disk slow", func(c *v1.RuntimeSettings) { c.DiskIntervalSeconds = 3601 }, false},
		{"too many disks", func(c *v1.RuntimeSettings) {
			c.DiskPaths = []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g", "/h", "/i"}
		}, false},
		{"relative disk", func(c *v1.RuntimeSettings) { c.DiskPaths = []string{"relative/data"} }, false},
		{"drive relative disk", func(c *v1.RuntimeSettings) { c.DiskPaths = []string{"C:relative"} }, false},
		{"oversized disk", func(c *v1.RuntimeSettings) { c.DiskPaths = []string{"/" + strings.Repeat("a", 1024)} }, false},
		{"report expansion", func(c *v1.RuntimeSettings) {
			c.DiskPaths = make([]string, 4)
			for i := range c.DiskPaths {
				c.DiskPaths[i] = "/" + strings.Repeat("<", 1000) + string(rune('a'+i))
			}
		}, false},
		{"duplicate disk", func(c *v1.RuntimeSettings) { c.DiskPaths = []string{"/", "/"} }, false},
		{"NUL disk", func(c *v1.RuntimeSettings) { c.DiskPaths = []string{"/data\x00suffix"} }, false},
		{"too many interfaces", func(c *v1.RuntimeSettings) { c.Interfaces = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} }, false},
		{"duplicate interface", func(c *v1.RuntimeSettings) { c.Interfaces = []string{"eth0", "eth0"} }, false},
		{"oversized interface", func(c *v1.RuntimeSettings) { c.Interfaces = []string{strings.Repeat("a", 256)} }, false},
		{"control interface", func(c *v1.RuntimeSettings) { c.Interfaces = []string{"eth0\n"} }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := runtimeSettings(10)
			test.mutate(&next)
			if err := next.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate = %v, valid=%v", err, test.valid)
			}
			collector, err := collect.New(collect.Options{Config: runtimeSettings(10)})
			if err != nil {
				t.Fatal(err)
			}
			if err := collector.SetConfig(next); (err == nil) != test.valid {
				t.Fatalf("SetConfig = %v, valid=%v", err, test.valid)
			}
		})
	}
}
