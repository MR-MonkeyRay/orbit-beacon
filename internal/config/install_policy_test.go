package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
)

// 策略路径的上级目录不能允许组或其他用户写入，也不能为符号链接。
// 使用用户私有主目录，而不是可写的 /tmp。
func securePolicyDirectory(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX install policy ownership")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".orbitbeacon-policy-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestReadInstallPolicyAcceptsExpectedOwnerAndDecodesDescriptor(t *testing.T) {
	path := filepath.Join(securePolicyDirectory(t), "install-policy.yaml")
	if err := os.WriteFile(path, []byte("server_url: https://trusted.example.test\nexpected_uid: 1000\nexpected_gid: 1000\ninstall_mode: native\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := config.ReadInstallPolicy(path, os.Geteuid())
	if err != nil {
		t.Fatalf("secure current-owner policy rejected: %v", err)
	}
	if got.ServerURL != "https://trusted.example.test" || got.ExpectedUID != 1000 || got.InstallMode != "native" {
		t.Fatalf("descriptor contents not decoded: %+v", got)
	}
	if _, err := config.ReadInstallPolicy(path, os.Geteuid()+1); err == nil {
		t.Fatal("policy owned by an unexpected administrator accepted")
	}
}

func TestReadInstallPolicyRejectsUnsafeFilesAndAncestors(t *testing.T) {
	for _, name := range []string{"final symlink", "ancestor symlink", "group writable file", "other writable file", "group writable ancestor", "other writable ancestor", "directory", "malformed YAML"} {
		t.Run(name, func(t *testing.T) {
			root := securePolicyDirectory(t)
			parent := filepath.Join(root, "policy")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "install-policy.yaml")
			if err := os.WriteFile(path, []byte("server_url: https://trusted.example.test\n"), 0600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "final symlink":
				link := filepath.Join(parent, "link.yaml")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "ancestor symlink":
				link := filepath.Join(root, "link")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "install-policy.yaml")
			case "group writable file":
				if err := os.Chmod(path, 0620); err != nil {
					t.Fatal(err)
				}
			case "other writable file":
				if err := os.Chmod(path, 0602); err != nil {
					t.Fatal(err)
				}
			case "group writable ancestor":
				if err := os.Chmod(parent, 0720); err != nil {
					t.Fatal(err)
				}
			case "other writable ancestor":
				if err := os.Chmod(parent, 0702); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path = parent
			case "malformed YAML":
				if err := os.WriteFile(path, []byte("server_url: ["), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := config.ReadInstallPolicy(path, os.Geteuid()); err == nil {
				t.Fatalf("unsafe policy accepted: %s", name)
			}
		})
	}
}

func TestReadInstallPolicyRejectsInvalidYAMLAndUnknownFields(t *testing.T) {
	cases := invalidYAMLDocuments()
	cases["duplicate policy key"] = "expected_uid: 1000\nexpected_uid: 2000\n"
	cases["quoted UID"] = "expected_uid: \"1000\"\n"
	cases["floating UID"] = "expected_uid: 1000.0\n"
	cases["unknown field"] = "unexpected: private-value\n"
	cases["wrong scalar type"] = "server_url: true\n"
	cases["oversized"] = "#" + strings.Repeat("x", 64<<10) + "\n"
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(securePolicyDirectory(t), "install-policy.yaml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.ReadInstallPolicy(path, os.Geteuid()); err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("invalid policy must fail without revealing values: %v", err)
			}
		})
	}
}
