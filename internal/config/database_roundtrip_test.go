package config_test

import (
	"encoding/json"
	"testing"
	"time"

	"orbitbeacon/internal/config"
)

func TestDatabaseFailoverExplicitZeroAndRoundTrip(t *testing.T) {
	raw := `{"primary":{"driver":"mysql","address":"127.0.0.1:3306","user":"u","password_file":"/run/password"},"standby":{"driver":"sqlite"},"failover":{"max_loss_window":"0s"}}`
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(map[string]string{"ORBITBEACON_DATABASE": raw})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Failover.MaxLossWindow != 0 || cfg.Database.Failover.FailureTimeout != 30*time.Second {
		t.Fatalf("defaults erased explicit zero: %+v", cfg.Database)
	}
	encoded, err := json.Marshal(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	var again config.DatabaseConfig
	if err = json.Unmarshal(encoded, &again); err != nil {
		t.Fatalf("serialized effective configuration rejected: %s/%v", encoded, err)
	}
	if again.Failover.MaxLossWindow != 0 {
		t.Fatal("roundtrip changed effective values")
	}
	standalone, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(map[string]string{"ORBITBEACON_DATABASE": "{}"})})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(standalone.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &again); err != nil {
		t.Fatalf("standalone roundtrip rejected: %s/%v", encoded, err)
	}
	encoded, err = json.Marshal(config.DatabaseConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &again); err != nil {
		t.Fatalf("default endpoint roundtrip rejected: %s/%v", encoded, err)
	}
}

func TestDatabaseTransportIntentRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name       string
		tls        string
		serverName string
		caFile     string
	}{
		{name: "plaintext"},
		{name: "system roots", tls: `,"tls":{}`, serverName: "db.test"},
		{name: "private CA", tls: `,"tls":{"server_name":"verified.test","ca_file":"/run/mysql-ca.pem"}`, serverName: "verified.test", caFile: "/run/mysql-ca.pem"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/password"` + test.tls + `}}`
			var database config.DatabaseConfig
			if err := json.Unmarshal([]byte(raw), &database); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(database)
			if err != nil {
				t.Fatal(err)
			}
			var fields struct {
				Primary map[string]json.RawMessage `json:"primary"`
			}
			if err = json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields.Primary["tls"]; present != (test.tls != "") {
				t.Fatal("marshal changed TLS opt-in")
			}
			var again config.DatabaseConfig
			if err = json.Unmarshal(encoded, &again); err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []config.DatabaseEndpoint{database.Primary, again.Primary} {
				if test.tls == "" {
					if endpoint.TLS != nil {
						t.Fatal("omitted TLS enabled encryption")
					}
				} else if endpoint.TLS == nil || endpoint.TLS.ServerName != test.serverName || endpoint.TLS.CAFile != test.caFile {
					t.Fatal("explicit TLS verification changed")
				}
			}
		})
	}
}
