package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

func TestCLIRejectsUnsafeInstallPolicyBeforeRuntimeSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX administrator policy ownership")
	}
	for _, kind := range []string{"writable ancestor", "symlink ancestor", "non-administrator owner"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "non-administrator owner" && os.Geteuid() == 0 {
				t.Skip("current process already has administrator ownership; injected owner rejection is covered by config tests")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			base, err := os.MkdirTemp(home, ".orbitbeacon-cli-policy-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			parent := filepath.Join(base, "policy")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "install.json")
			data, err := json.Marshal(config.InstallPolicy{ServerURL: "https://beacon.example.test", StateDir: filepath.Join(base, "state"), ExpectedUID: 1000, ExpectedGID: 1000, InstallMode: "native"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "writable ancestor":
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink ancestor":
				link := filepath.Join(base, "link")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "install.json")
			}
			err = run([]string{"doctor", "--install-policy", path})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "install policy") {
				t.Fatalf("unsafe policy reached runtime/configuration setup instead of install policy rejection: %v", err)
			}
		})
	}
}
