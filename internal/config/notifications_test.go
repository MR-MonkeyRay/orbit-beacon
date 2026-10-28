package config_test

import (
	"orbitbeacon/internal/config"
	"testing"
)

func TestNotificationConfigurationSourcesAndTypes(t *testing.T) {
	for _, key := range []string{"allow_insecure_webhooks", "notification_allow_private_networks"} {
		env := "ORBITBEACON_ALLOW_INSECURE_WEBHOOKS"
		if key == "notification_allow_private_networks" {
			env = "ORBITBEACON_NOTIFICATION_ALLOW_PRIVATE_NETWORKS"
		}
		opts := config.LoadOptions{LookupEnv: lookup(nil)}
		cfg, e := config.LoadServer(opts)
		if e != nil || cfg.AllowInsecureWebhooks || cfg.NotificationAllowPrivateNetworks {
			t.Fatal("unsafe default")
		}
		opts.File = configFile(t, map[string]any{key: true})
		cfg, e = config.LoadServer(opts)
		if e != nil || !(cfg.AllowInsecureWebhooks || cfg.NotificationAllowPrivateNetworks) {
			t.Fatal("file value not loaded", e)
		}
		opts.LookupEnv = lookup(map[string]string{env: "false"})
		cfg, e = config.LoadServer(opts)
		if e != nil || cfg.AllowInsecureWebhooks || cfg.NotificationAllowPrivateNetworks {
			t.Fatal("environment precedence", e)
		}
		opts.Flags = map[string]string{key: "true"}
		cfg, e = config.LoadServer(opts)
		if e != nil || !(cfg.AllowInsecureWebhooks || cfg.NotificationAllowPrivateNetworks) {
			t.Fatal("flag precedence", e)
		}
		for _, source := range []string{"file", "env", "flag"} {
			bad := config.LoadOptions{LookupEnv: lookup(nil)}
			switch source {
			case "file":
				bad.File = configFile(t, map[string]any{key: "enabled"})
			case "env":
				bad.LookupEnv = lookup(map[string]string{env: "enabled"})
			case "flag":
				bad.Flags = map[string]string{key: "enabled"}
			}
			if _, e = config.LoadServer(bad); e == nil {
				t.Fatalf("accepted invalid %s %s", source, key)
			}
		}
	}
}
