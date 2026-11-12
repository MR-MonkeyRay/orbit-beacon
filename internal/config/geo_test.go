package config_test

import (
	"testing"

	"orbitbeacon/internal/config"
)

func TestGeoConfiguration(t *testing.T) {
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(nil)})
	if err != nil || !cfg.GeoIPLookup {
		t.Fatalf("default %+v %v", cfg, err)
	}
	cfg, err = config.LoadServer(config.LoadOptions{LookupEnv: lookup(map[string]string{"ORBITBEACON_GEOIP_LOOKUP": "false", "ORBITBEACON_GEOIP_TOKEN": "token"})})
	if err != nil || cfg.GeoIPLookup || cfg.GeoIPToken != "token" {
		t.Fatalf("env %+v %v", cfg, err)
	}
	for _, env := range []map[string]string{{"ORBITBEACON_GEOIP_LOOKUP": "bad"}, {"ORBITBEACON_GEOIP_TOKEN": "token", "ORBITBEACON_GEOIP_TOKEN_FILE": "/run/secret"}} {
		if _, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(env)}); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
