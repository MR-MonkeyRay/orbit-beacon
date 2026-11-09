package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestDatabaseConfigurationReplacesWholeLowerPriorityObject(t *testing.T) {
	file := configFile(t, map[string]any{"database": map[string]any{"primary": map[string]any{"driver": "mysql", "address": "db.test:3306", "user": "db-user", "password_file": "/run/secret"}, "standby": map[string]any{"driver": "sqlite"}, "failover": map[string]any{"failure_timeout": "30s"}}})
	cfg, err := config.LoadServer(config.LoadOptions{File: file, LookupEnv: lookup(map[string]string{"ORBITBEACON_DATABASE": "{}"})})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "db.test") || strings.Contains(string(data), "db-user") {
		t.Fatalf("lower priority endpoint survived replacement: %s", data)
	}
}

func TestDatabasePlaintextEndpointReplacesLowerPriorityTLS(t *testing.T) {
	lower := `{"primary":{"driver":"mysql","address":"tls.test:3306","user":"tls-user","password_file":"/run/tls-password","tls":{"server_name":"tls.test","ca_file":"/run/old-ca.pem"}},"standby":{"driver":"sqlite"}}`
	plain := `{"primary":{"driver":"mysql","address":"plain.test:3306","user":"plain-user","password_file":"/run/plain-password"}}`
	file := configFile(t, map[string]any{"database": json.RawMessage(lower)})
	for _, source := range []string{"environment", "flag"} {
		t.Run(source, func(t *testing.T) {
			opts := config.LoadOptions{File: file, LookupEnv: lookup(map[string]string{"ORBITBEACON_DATABASE": plain})}
			if source == "flag" {
				opts.LookupEnv = lookup(map[string]string{"ORBITBEACON_DATABASE": lower})
				opts.Flags = map[string]string{"database": plain}
			}
			cfg, err := config.LoadServer(opts)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Database.Primary.Address != "plain.test:3306" || cfg.Database.Primary.User != "plain-user" || cfg.Database.Primary.PasswordFile != "/run/plain-password" || cfg.Database.Primary.TLS != nil || cfg.Database.Standby != nil {
				t.Fatal("plaintext endpoint retained lower-priority configuration")
			}
		})
	}
}

func TestDatabaseConfigurationValidatesOnlySelectedObject(t *testing.T) {
	file := configFile(t, map[string]any{"database": map[string]any{"primary": map[string]any{"driver": "mysql", "unknown": "private-value"}}})
	if _, err := config.LoadServer(config.LoadOptions{File: file, LookupEnv: lookup(map[string]string{"ORBITBEACON_DATABASE": "{}"})}); err != nil {
		t.Fatalf("replaced value was validated: %v", err)
	}
	for _, raw := range []string{
		`null`, `[]`, `{"unknown":true}`, `{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","tls":{"insecure":true}}}`,
		`{"standby":{}}`, `{"standby":null}`, `{"primary":{"driver":"sqlite","address":"db.test:3306"}}`, `{"failover":{"failure_timeout":"30s"}}`,
		`{"primary":{"driver":"sqlite","address":""}}`, `{"primary":{"driver":"sqlite","tls":null}}`, `{"standby":{ }}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret"},"standby":{"driver":"sqlite"},"failover":{"failure_timeout":null}}`,
		`{"primary":{"driver":""}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","tls":{"ca_file":null}}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","tls":null}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","tls":false}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","tls":{"server_name":null}}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","tls":{"server_name":"bad\nname"}}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","tls":{"insecure_skip_verify":true}}}`,
		`{"primary":{"driver":"sqlite","tls":{}}}`,
		`{"primary":{"driver":"mysql","network":"unix","address":"relative.sock","user":"u","password_file":"/run/secret"}}`,
		`{"primary":{"driver":"mysql","network":"unix","address":"/run/mysql.sock","user":"u","password_file":"/run/secret","tls":{}}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret","main_database":"same","history_database":"same"}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret"},"standby":{"driver":"sqlite"},"failover":{"failure_timeout":"0s"}}`,
		`{"primary":{"driver":"mysql","address":"db.test:3306","user":"u","password_file":"/run/secret"},"standby":{"driver":"sqlite"},"failover":{"max_loss_window":"-1s"}}`,
	} {
		if _, err := config.LoadServer(config.LoadOptions{LookupEnv: lookup(map[string]string{"ORBITBEACON_DATABASE": raw})}); err == nil {
			t.Fatalf("accepted invalid database object %s", raw)
		}
	}
}
