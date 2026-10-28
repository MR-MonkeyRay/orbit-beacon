package config_test

import (
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestServerObservabilityConfiguration(t *testing.T) {
	file := configFile(t, map[string]any{
		"access_log":     true,
		"metrics_listen": "127.0.0.1:9464",
	})
	for _, tt := range []struct {
		name        string
		file        string
		env         map[string]string
		flags       map[string]string
		wantAccess  bool
		wantMetrics string
	}{
		{name: "disabled by default"},
		{name: "JSON", file: file, wantAccess: true, wantMetrics: "127.0.0.1:9464"},
		{name: "environment overrides JSON", file: file, env: map[string]string{
			"ORBITBEACON_ACCESS_LOG":     "false",
			"ORBITBEACON_METRICS_LISTEN": "127.0.0.1:9465",
		}, wantMetrics: "127.0.0.1:9465"},
		{name: "flags override environment", file: file, env: map[string]string{
			"ORBITBEACON_ACCESS_LOG":     "false",
			"ORBITBEACON_METRICS_LISTEN": "127.0.0.1:9465",
		}, flags: map[string]string{
			"access_log":     "true",
			"metrics_listen": "127.0.0.1:9466",
		}, wantAccess: true, wantMetrics: "127.0.0.1:9466"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.LoadServer(config.LoadOptions{
				File: tt.file, LookupEnv: lookup(tt.env), Flags: tt.flags,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.AccessLog != tt.wantAccess || got.MetricsListen != tt.wantMetrics {
				t.Errorf("observability configuration = (%t, %q), want (%t, %q)", got.AccessLog, got.MetricsListen, tt.wantAccess, tt.wantMetrics)
			}
		})
	}
}

func TestServerObservabilityConfigurationRejectsInvalidValues(t *testing.T) {
	for _, tt := range []struct {
		name  string
		key   string
		env   string
		value string
	}{
		{name: "non-boolean access log", key: "access_log", env: "ORBITBEACON_ACCESS_LOG", value: "enabled"},
		{name: "invalid metrics address", key: "metrics_listen", env: "ORBITBEACON_METRICS_LISTEN", value: "localhost"},
		{name: "metrics shares the default listener", key: "metrics_listen", env: "ORBITBEACON_METRICS_LISTEN", value: "127.0.0.1:8080"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range []string{"JSON", "environment", "flag"} {
				t.Run(source, func(t *testing.T) {
					opts := config.LoadOptions{LookupEnv: lookup(nil)}
					switch source {
					case "JSON":
						opts.File = configFile(t, map[string]any{tt.key: tt.value})
					case "environment":
						opts.LookupEnv = lookup(map[string]string{tt.env: tt.value})
					case "flag":
						opts.Flags = map[string]string{tt.key: tt.value}
					}
					_, err := config.LoadServer(opts)
					if err == nil || !strings.Contains(err.Error(), tt.key) {
						t.Fatalf("error = %v, want invalid %s", err, tt.key)
					}
				})
			}
		})
	}
}
