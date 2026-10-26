package service_test

import (
	"path/filepath"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
)

func TestClientRejectsServerURLPathsInsteadOfSilentlyDiscardingThem(t *testing.T) {
	state, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := service.OpenConfigStore(t.Context(), filepath.Join(t.TempDir(), "desired.json"), desired(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		"https://beacon.example.test/prefix", "https://beacon.example.test/prefix/",
		"https://beacon.example.test//", "https://beacon.example.test/%2f",
		"https://beacon.example.test/./", "https://beacon.example.test/?query=1",
		"https://beacon.example.test/?", "https://user@beacon.example.test/",
		"http://127.0.0.1:8080/prefix", "http://127.0.0.1:8080/prefix/",
	} {
		t.Run(url, func(t *testing.T) {
			client, err := service.NewClient(service.ClientOptions{ServerURL: url, Identity: state, Config: cfg, AllowInsecureLoopback: true})
			if err == nil {
				client.Close()
				t.Fatalf("unsafe/non-root URL %q silently accepted", url)
			}
		})
	}
	for _, url := range []string{"https://beacon.example.test", "https://beacon.example.test/", "http://127.0.0.1:8080/"} {
		client, err := service.NewClient(service.ClientOptions{ServerURL: url, Identity: state, Config: cfg, AllowInsecureLoopback: true})
		if err != nil {
			t.Fatalf("root URL %q rejected: %v", url, err)
		}
		client.Close()
	}
}
