package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
			path := filepath.Join(parent, "install-policy.yaml")
			data := []byte(fmt.Sprintf("server_url: https://beacon.example.test\nstate_dir: %q\nexpected_uid: 1000\nexpected_gid: 1000\ninstall_mode: native\n", filepath.Join(base, "state")))
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
				path = filepath.Join(link, "install-policy.yaml")
			}
			err = run([]string{"doctor", "--install-policy", path})
			if err == nil {
				t.Fatalf("unsafe policy reached runtime/configuration setup instead of install policy rejection: %v", err)
			}
		})
	}
}
