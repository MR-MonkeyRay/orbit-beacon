package service_test

import (
	"path/filepath"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
)

func TestClientRejectsServerURLPathsInsteadOfSilentlyDiscardingThem(t *testing.T) {
	state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		"https://beacon.example.test/prefix", "https://beacon.example.test/prefix/",
		"https://beacon.example.test//", "https://beacon.example.test/%2f",
		"https://beacon.example.test/./", "https://beacon.example.test/?query=1",
		"https://beacon.example.test/?", "https://user@beacon.example.test/",
		"http://127.0.0.1:8080/prefix", "http://127.0.0.1:8080/prefix/",
		"http://beacon.example.test//", "http://beacon.example.test/%2f",
		"http://beacon.example.test/?", "http://beacon.example.test/#",
		"http://user@beacon.example.test/",
		"http://beacon.example.test:65536/", "ftp://beacon.example.test/",
		"//beacon.example.test/",
	} {
		t.Run(url, func(t *testing.T) {
			client, err := service.NewClient(service.ClientOptions{ServerURL: url, Identity: state})
			if err == nil {
				client.Close()
				t.Fatalf("unsafe/non-root URL %q silently accepted", url)
			}
		})
	}
}

func TestClientAcceptsHTTPAndHTTPSServerOrigins(t *testing.T) {
	state, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		"http://beacon.example.test", "http://beacon.example.test/",
		"http://beacon.example.test:8080", "http://192.0.2.10:8080/",
		"http://[2001:db8::1]:8080/", "http://127.0.0.1:8080/",
		"https://beacon.example.test", "https://beacon.example.test/",
	} {
		t.Run(url, func(t *testing.T) {
			client, err := service.NewClient(service.ClientOptions{ServerURL: url, Identity: state})
			if err != nil {
				t.Fatalf("root URL %q rejected: %v", url, err)
			}
			client.Close()
		})
	}
}
