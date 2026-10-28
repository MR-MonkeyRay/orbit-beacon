package config_test

import (
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestServerWebUpdateIntervalLayers(t *testing.T) {
	file := configFile(t, map[string]any{"web_update_interval_seconds": 2})
	for _, test := range []struct {
		name  string
		file  string
		env   map[string]string
		flags map[string]string
		want  int
	}{
		{name: "default browser cadence", want: 10},
		{name: "JSON minimum", file: file, want: 2},
		{name: "environment overrides JSON", file: file, env: map[string]string{"ORBITBEACON_WEB_UPDATE_INTERVAL_SECONDS": "30"}, want: 30},
		{name: "explicit maximum overrides environment", file: file, env: map[string]string{"ORBITBEACON_WEB_UPDATE_INTERVAL_SECONDS": "30"}, flags: map[string]string{"web_update_interval_seconds": "60"}, want: 60},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := config.LoadServer(config.LoadOptions{File: test.file, LookupEnv: lookup(test.env), Flags: test.flags})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WebUpdateIntervalSeconds != test.want {
				t.Fatalf("cadence=%d want=%d", cfg.WebUpdateIntervalSeconds, test.want)
			}
		})
	}
}

func TestServerWebUpdateIntervalRejectsInvalidSelection(t *testing.T) {
	for _, value := range []any{nil, 0, 1, 61, -2, 2.5, true, "10", []any{}, map[string]any{}} {
		_, err := config.LoadServer(config.LoadOptions{File: configFile(t, map[string]any{"web_update_interval_seconds": value}), LookupEnv: lookup(nil)})
		if err == nil || !strings.Contains(err.Error(), "web_update_interval_seconds") {
			t.Fatalf("JSON value %#v error=%v", value, err)
		}
	}
	for _, value := range []string{"", "0", "1", "61", "-2", "2.5", "true", "null"} {
		for _, source := range []string{"environment", "flag"} {
			opts := config.LoadOptions{LookupEnv: lookup(nil)}
			if source == "environment" {
				opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_WEB_UPDATE_INTERVAL_SECONDS": value})
			} else {
				opts.Flags = map[string]string{"web_update_interval_seconds": value}
			}
			_, err := config.LoadServer(opts)
			if err == nil || !strings.Contains(err.Error(), "web_update_interval_seconds") {
				t.Fatalf("%s value %q error=%v", source, value, err)
			}
		}
	}
}
